package middleware

import (
	"context"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestBloom spins up an in-process Redis and a fresh bloom filter backed
// by it. Each test gets isolated state — no leakage between cases.
func newTestBloom(t *testing.T, m, k uint) (*BloomFilter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewBloomFilter(rdb, "bloom:test", m, k), mr
}

// TestBloom_NoFalseNegatives is the load-bearing property of a Bloom filter:
// anything Added must subsequently MightContain → true. False positives are
// acceptable; a false negative would silently let a duplicate webhook
// through, double-debiting an account.
func TestBloom_NoFalseNegatives(t *testing.T) {
	bf, _ := newTestBloom(t, 10_000, 7)
	ctx := context.Background()

	items := []string{"ITEM-aaaa", "ITEM-bbbb", "ITEM-cccc", "ITEM-dddd"}
	for _, it := range items {
		require.NoError(t, bf.Add(ctx, it))
	}
	for _, it := range items {
		seen, err := bf.MightContain(ctx, it)
		require.NoError(t, err)
		assert.True(t, seen, "%q was added but bloom claims unseen — false negative is a correctness bug", it)
	}
}

// TestBloom_RejectsNotAdded sanity-checks that fresh items aren't reported
// as seen at low load factor. With m=100k and k=7 at zero load we expect
// zero false positives across a handful of probes.
func TestBloom_RejectsNotAdded(t *testing.T) {
	bf, _ := newTestBloom(t, 100_000, 7)
	ctx := context.Background()

	for _, it := range []string{"never-added-1", "never-added-2", "never-added-3"} {
		seen, err := bf.MightContain(ctx, it)
		require.NoError(t, err)
		assert.False(t, seen, "%q was never added but bloom reports seen at zero load", it)
	}
}

// TestBloom_FalsePositiveRateUnderTargetLoad is a statistical sanity check.
// At m=100k, k=7, inserting 10,000 items targets ~1% FP rate. We allow a
// generous 5% upper bound so a flaky run on a slow CI doesn't fail; gross
// misconfiguration (e.g. too few hashes) will exceed even that.
func TestBloom_FalsePositiveRateUnderTargetLoad(t *testing.T) {
	bf, _ := newTestBloom(t, 100_000, 7)
	ctx := context.Background()

	const inserted = 10_000
	for i := 0; i < inserted; i++ {
		require.NoError(t, bf.Add(ctx, "in-"+strconv.Itoa(i)))
	}

	const probes = 5_000
	falsePositives := 0
	for i := 0; i < probes; i++ {
		seen, err := bf.MightContain(ctx, "out-"+strconv.Itoa(i))
		require.NoError(t, err)
		if seen {
			falsePositives++
		}
	}
	rate := float64(falsePositives) / float64(probes)
	t.Logf("false-positive rate over %d probes: %.4f (%d hits)", probes, rate, falsePositives)
	assert.Less(t, rate, 0.05, "bloom FP rate %.4f exceeds 5%% — sizing is wrong", rate)
}

// TestBloom_PositionsDeterministic locks in the double-hashing invariant:
// the same key must hash to the same bit positions every time, otherwise
// MightContain would have its own intrinsic false-negative rate.
func TestBloom_PositionsDeterministic(t *testing.T) {
	bf, _ := newTestBloom(t, 10_000, 7)
	a := bf.positions("ITEM-determinism-check")
	b := bf.positions("ITEM-determinism-check")
	assert.Equal(t, a, b, "positions must be deterministic — same input, same bits")
	assert.Len(t, a, int(bf.k))
}

// TestBloom_PipelineRoundTrip exercises the Redis pipeline path end-to-end.
// Both Add and MightContain pipeline k Redis commands; this confirms the
// pipeline result handling correctly maps to per-bit values.
func TestBloom_PipelineRoundTrip(t *testing.T) {
	bf, mr := newTestBloom(t, 1024, 4)
	ctx := context.Background()

	require.NoError(t, bf.Add(ctx, "pipeline-1"))
	require.NoError(t, bf.Add(ctx, "pipeline-2"))

	seen, err := bf.MightContain(ctx, "pipeline-1")
	require.NoError(t, err)
	assert.True(t, seen)

	// The Redis bitfield key must exist after Adds — proves the pipeline
	// actually committed rather than silently dropping the writes.
	assert.True(t, mr.Exists(bf.key), "bloom Redis key must be created by Add")
}
