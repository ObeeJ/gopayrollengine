//go:build integration

package middleware

import (
	"os"
	"testing"
)

// To run: REDIS_URL=... go test -tags=integration ./internal/api/middleware/...
func TestIdempotency_Placeholder(t *testing.T) {
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("REDIS_URL not set — skipping idempotency integration test")
	}
	// TODO: cache miss processes handler; cache hit replays; 4xx not cached.
}

func TestBloomFilter_Placeholder(t *testing.T) {
	if os.Getenv("REDIS_URL") == "" {
		t.Skip("REDIS_URL not set — skipping bloom-filter integration test")
	}
	// TODO: Add(item) then MightContain(item)==true; un-added item==false.
}
