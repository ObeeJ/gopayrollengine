package models

import (
	"context"
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

	// Migrations need DDL privileges (CREATE TABLE, ALTER, etc.) the
	// application role intentionally lacks. If MIGRATION_DATABASE_URL is set
	// — typically pointing at the superuser — use it for the schema upgrade
	// and then fall back to the unprivileged DSN for the running app.
	migrationDSN := os.Getenv("MIGRATION_DATABASE_URL")
	if migrationDSN == "" {
		migrationDSN = dsn
	}
	runMigrations(migrationDSN)

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

// ScopedDB returns a DB instance pre-filtered to the caller's organisation
// via an explicit WHERE clause. This is the convention-based isolation path —
// works without any DB-side configuration, but only holds if every developer
// remembers to call it. New code should prefer WithOrgScope, which gets the
// same isolation enforced structurally by Postgres RLS.
func ScopedDB(orgID string) *gorm.DB {
	return DB.Where("organization_id = ?", orgID)
}

// WithOrgScope runs fn inside a transaction whose Postgres session has
// app.org_id set to orgID. The RLS policies defined in migration 000008 then
// filter every SELECT/INSERT/UPDATE/DELETE to that org, regardless of whether
// the application code remembers to add a WHERE clause. This is structural
// tenant isolation: forgetting the filter yields zero rows, not a leak.
//
// Use this for any tenant-scoped operation that isn't already covered by the
// repository layer (ad-hoc queries, batch jobs, cross-repo aggregates).
// Wraps the work in a transaction because SET LOCAL is scoped to the
// transaction — without that boundary the session variable would leak to the
// next query the connection pool hands out.
func WithOrgScope(ctx context.Context, orgID string, fn func(tx *gorm.DB) error) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// set_config(name, value, is_local=true) is the parameterised form of
		// SET LOCAL — accepts a $1 placeholder where SET LOCAL does not.
		if err := tx.Exec("SELECT set_config('app.org_id', ?, true)", orgID).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}
