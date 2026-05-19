package workers

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// RDB — shared Redis client for idempotency, bloom, rate limiting, and whatever's next.
var RDB *redis.Client

// InitRedisClient — connects and pings; dies on failure because half the safety nets need Redis.
func InitRedisClient() {
	addr := os.Getenv("REDIS_URL")
	if addr == "" {
		addr = "localhost:6379"
	}

	RDB = redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     os.Getenv("REDIS_PASSWORD"),
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := RDB.Ping(ctx).Err(); err != nil {
		log.Fatalf("FATAL: cannot connect to Redis at %q: %v", addr, err) //nolint:gosec // addr is from env; err is from library
	}

	log.Println("Redis client initialised.")
}
