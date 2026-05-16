package middleware

import (
	"context"
	"hash/fnv"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// BloomFilter is a probabilistic data structure for set membership testing.
//
// DSA: Bloom Filter — uses K independent hash functions to set K bits in a
// Redis bitfield. Membership test checks all K bits:
//   - All bits set   → "probably seen" (false positive rate ~1% at configured size)
//   - Any bit unset  → "definitely not seen" (no false negatives)
//
// This lets us skip the DB read for ~99% of duplicate webhook events at O(1) cost.
// Size: m=100,000 bits (~12 KB Redis memory), k=7 hash functions → ~1% FP rate
// at 10,000 inserted elements.
type BloomFilter struct {
	rdb    *redis.Client
	key    string // Redis key for the bitfield
	m      uint   // number of bits
	k      uint   // number of hash functions
}

// NewBloomFilter creates a BloomFilter backed by a Redis bitfield.
// key is the Redis key, m is the bit array size, k is the number of hash functions.
func NewBloomFilter(rdb *redis.Client, key string, m, k uint) *BloomFilter {
	return &BloomFilter{rdb: rdb, key: key, m: m, k: k}
}

// WebhookBloom is the singleton bloom filter for webhook transaction references.
// Initialised in routes.go once the Redis client is available.
var WebhookBloom *BloomFilter

// positions returns the k bit positions for a given item using double hashing.
// Double hashing: pos_i = (h1 + i*h2) mod m
// This simulates k independent hash functions using only 2 actual hash computations.
func (bf *BloomFilter) positions(item string) []uint {
	h1 := fnv.New64a()
	h1.Write([]byte(item))
	a := h1.Sum64()

	h2 := fnv.New64()
	h2.Write([]byte(item))
	b := h2.Sum64()

	positions := make([]uint, bf.k)
	for i := uint(0); i < bf.k; i++ {
		positions[i] = uint((a + uint64(i)*b) % uint64(bf.m))
	}
	return positions
}

// Add sets the k bits for item in the Redis bitfield — O(k) Redis SETBIT calls,
// pipelined into a single round-trip.
func (bf *BloomFilter) Add(ctx context.Context, item string) error {
	pipe := bf.rdb.Pipeline()
	for _, pos := range bf.positions(item) {
		pipe.SetBit(ctx, bf.key, int64(pos), 1)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// MightContain returns true if item was probably already seen (all k bits set).
// Returns false if item was definitely not seen (any bit is 0).
// O(k) Redis GETBIT calls, pipelined.
func (bf *BloomFilter) MightContain(ctx context.Context, item string) (bool, error) {
	pipe := bf.rdb.Pipeline()
	cmds := make([]*redis.IntCmd, len(bf.positions(item)))
	for i, pos := range bf.positions(item) {
		cmds[i] = pipe.GetBit(ctx, bf.key, int64(pos))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	for _, cmd := range cmds {
		if cmd.Val() == 0 {
			return false, nil // definitely not seen
		}
	}
	return true, nil // probably seen
}

// bloomBitSize converts an integer to string for Redis key namespacing.
// Exported for use in tests.
func BloomBitKey(n uint) string {
	return strconv.FormatUint(uint64(n), 10)
}
