package workers

import (
	"log"
	"os"

	"github.com/hibiken/asynq"
)

var Client *asynq.Client

// InitAsynqClient — creates the global Asynq client; defaults to localhost:6379.
func InitAsynqClient() {
	redisAddr := os.Getenv("REDIS_URL")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	Client = asynq.NewClient(asynq.RedisClientOpt{
		Addr:     redisAddr,
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	log.Println("Asynq client initialized.")
}

// CloseAsynqClient — flushes and closes the client; defer it in main.
func CloseAsynqClient() {
	if Client != nil {
		Client.Close()
	}
}
