package models

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

	// Migrations need DDL the app role doesn't have; MIGRATION_DATABASE_URL overrides for the upgrade.
	migrationDSN := os.Getenv("MIGRATION_DATABASE_URL")
	if migrationDSN == "" {
		migrationDSN = dsn
	}
	runMigrations(migrationDSN)

	// Cap connections so 500 concurrent webhooks can't exhaust postgres max_connections.
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal("Failed to get underlying sql.DB:", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	fmt.Println("Database ready.")
	DB = db
}

// runMigrations — applies pending versioned SQL migrations; reviewable beats AutoMigrate.
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

// ScopedDB — legacy convention-based filter; new code should use WithOrgScope and let RLS do it.
func ScopedDB(orgID string) *gorm.DB {
	return DB.Where("organization_id = ?", orgID)
}

// WithOrgScope runs fn in a tx with app.org_id=orgID so RLS does the tenant filtering for you.
func WithOrgScope(ctx context.Context, orgID string, fn func(tx *gorm.DB) error) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// set_config(..., true) is SET LOCAL's parameterised cousin — accepts placeholders.
		if err := tx.Exec("SELECT set_config('app.org_id', ?, true)", orgID).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}
