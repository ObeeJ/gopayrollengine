package workers

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// RDB is the shared Redis client used by the idempotency middleware,
// bloom filter, and any future Redis-backed features.
var RDB *redis.Client

// InitRedisClient initialises the shared Redis client and verifies
// connectivity with a ping. Exits the process on failure — the app
// cannot enforce idempotency or rate limits without Redis.
func InitRedisClient() {
	addr := os.Getenv("REDIS_URL")
	if addr == "" {
		addr = "localhost:6379"
	}

	RDB = redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := RDB.Ping(ctx).Err(); err != nil {
		log.Fatalf("FATAL: cannot connect to Redis at %s: %v", addr, err)
	}

	log.Println("Redis client initialised.")
}
