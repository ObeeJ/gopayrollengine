package middleware

import (
	"context"
	"hash/fnv"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// BloomFilter — Redis-backed bloom for O(1) duplicate-webhook detection; ~1% FP, zero FN.
type BloomFilter struct {
	rdb    *redis.Client
	key    string // Redis key for the bitfield
	m      uint   // number of bits
	k      uint   // number of hash functions
}

// NewBloomFilter — builds a bloom filter at Redis key with m bits and k hashes.
func NewBloomFilter(rdb *redis.Client, key string, m, k uint) *BloomFilter {
	return &BloomFilter{rdb: rdb, key: key, m: m, k: k}
}

// WebhookBloom — singleton bloom for webhook tx refs; wired up in routes.go.
var WebhookBloom *BloomFilter

// positions — k bit indices via double hashing: pos_i = (h1 + i*h2) mod m.
func (bf *BloomFilter) positions(item string) []uint {
	h1 := fnv.New64a()
	_, _ = h1.Write([]byte(item)) // hash.Hash.Write never returns a non-nil error
	a := h1.Sum64()

	h2 := fnv.New64()
	_, _ = h2.Write([]byte(item))
	b := h2.Sum64()

	positions := make([]uint, bf.k)
	for i := uint(0); i < bf.k; i++ {
		positions[i] = uint((a + uint64(i)*b) % uint64(bf.m))
	}
	return positions
}

// Add sets the k bits for item — pipelined SETBITs in one round-trip.
func (bf *BloomFilter) Add(ctx context.Context, item string) error {
	pipe := bf.rdb.Pipeline()
	for _, pos := range bf.positions(item) {
		pipe.SetBit(ctx, bf.key, int64(pos), 1)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// MightContain — true if all k bits are set (probably seen), false if any is 0 (definitely not).
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

// BloomBitKey — stringifies n for Redis key namespacing; exported for tests.
func BloomBitKey(n uint) string {
	return strconv.FormatUint(uint64(n), 10)
}
