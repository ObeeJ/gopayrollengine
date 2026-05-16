package models

import (
	"fmt"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// InitDB — connects, migrates, and hands you a DB or kills the process trying.
func InitDB() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "host=localhost user=postgres password=postgres dbname=payroll_db port=5432 sslmode=disable"
	}

	// Silence GORM's query logger in production; it leaks query structure into logs.
	logLevel := logger.Info
	if os.Getenv("APP_ENV") == "production" {
		logLevel = logger.Silent
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		log.Fatal("DB connection refused — is Postgres running?", err)
	}

	runMigrations(dsn)

	fmt.Println("Database ready.")
	DB = db
}

// runMigrations — applies all pending versioned SQL migrations from the migrations folder.
// Versioned migrations beat AutoMigrate because they're reviewable, reversible, and auditable.
func runMigrations(dsn string) {
	migrationsPath := os.Getenv("MIGRATIONS_PATH")
	if migrationsPath == "" {
		migrationsPath = "file://internal/db/migrations"
	}

	m, err := migrate.New(migrationsPath, dsn)
	if err != nil {
		log.Fatal("Migration init failed — check your migrations folder and DSN:", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("Migration failed in production — refusing to start with a dirty schema:", err)
		}
		// In dev, log and continue — lets you work with a partial schema during development.
		log.Printf("WARNING: migration error (non-fatal in dev): %v", err)
	}
}

// ScopedDB — returns a DB instance pre-filtered to the caller's organization.
// Every query that touches tenant data must go through here; raw models.DB is for migrations only.
func ScopedDB(orgID string) *gorm.DB {
	return DB.Where("organization_id = ?", orgID)
}
