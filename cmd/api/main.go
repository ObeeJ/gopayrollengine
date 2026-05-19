package main

import (
	"context"
	"go-payroll-engine/internal/api"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/config"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load() // .env is optional; production uses real env vars

	// Production refuses to start with mock money, missing encryption, or unsigned tokens.
	if os.Getenv("APP_ENV") == "production" && os.Getenv("MOCK_MODE") == "true" {
		log.Fatal("FATAL: MOCK_MODE=true is not allowed in production. Refusing to start.")
	}
	if os.Getenv("APP_ENV") == "production" && os.Getenv("ENCRYPTION_KEK") == "" {
		log.Fatal("FATAL: ENCRYPTION_KEK is not set. PII would be stored in plaintext. Refusing to start.")
	}
	if os.Getenv("APP_ENV") == "production" && os.Getenv("JWT_SECRET") == "" {
		log.Fatal("FATAL: JWT_SECRET is not set. Tokens would be unsigned. Refusing to start.")
	}

	cfg := config.Load()

	models.InitEncryption() // must run before InitDB so the GORM serializer is ready
	models.InitDB()
	middleware.InitJWT()    // loads JWT secret after env is confirmed present
	workers.InitAsynqClient()
	workers.InitRedisClient()
	defer workers.CloseAsynqClient()

	switch cfg.AppMode {
	case "seed":
		models.SeedDB()
	case "worker":
		startWorker(cfg.RedisURL)
	case "collect-evidence":
		// Run the SOC 2 evidence collector for yesterday — wire this to a daily cron.
		collector := services.NewEvidenceCollector()
		if err := collector.Collect(time.Now().AddDate(0, 0, -1)); err != nil {
			log.Fatal("Evidence collection failed:", err)
		}
	default:
		startAPI(cfg.Port)
	}
}

func startAPI(port string) {
	r := api.SetupRouter()

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("Starting API server on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down API server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("API server exited gracefully")
}

func startWorker(redisAddr string) {
	srv := asynq.NewServer(
		asynq.RedisClientOpt{
			Addr:     redisAddr,
			Password: os.Getenv("REDIS_PASSWORD"),
		},
		asynq.Config{
			Concurrency: 10,
			Queues: map[string]int{
				"critical": 6,
				"default":  3,
				"low":      1,
			},
		},
	)

	mux := asynq.NewServeMux()
	mux.HandleFunc(workers.TypeProcessPayroll, workers.NewPayrollHandler().ProcessPayrollTask)
	mux.HandleFunc(workers.TypeVerifyBVN, workers.NewBVNHandler().ProcessBVNTask)

	log.Println("Worker server starting...")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.Run(mux); err != nil {
			log.Fatalf("could not run worker: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down worker...")
	srv.Shutdown()
	log.Println("Worker exited gracefully")
}
