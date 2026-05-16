package workers

import (
	"log"
	"os"

	"github.com/hibiken/asynq"
)

var Client *asynq.Client

// InitAsynqClient creates the global Asynq client used to enqueue background jobs.
// Defaults to localhost:6379 if REDIS_URL is not set.
func InitAsynqClient() {
	redisAddr := os.Getenv("REDIS_URL")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	Client = asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr})
	log.Println("Asynq client initialized.")
}

// CloseAsynqClient flushes and closes the Asynq client connection.
// Should be deferred in main to ensure clean shutdown.
func CloseAsynqClient() {
	if Client != nil {
		Client.Close()
	}
}
