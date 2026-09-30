//go:build integration

// Package testutil holds integration-test helpers shared across packages.
package testutil

import (
	"net/url"
	"os"
	"testing"

	"go-payroll-engine/internal/models"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const appRole = "rls_app_test"

// UseAppRoleDB swaps models.DB for the rest of the test to a connection
// logged in as a NOSUPERUSER NOBYPASSRLS role — the same posture as the
// production payroll_app role (config/postgres-init.sql).
//
// The integration suite otherwise connects as a superuser, and superusers
// bypass row-level security entirely. Anything that touches a tenant table
// outside models.WithOrgScope therefore passes every test while failing (or
// silently returning nothing) in production, where RLS is forced. Tests that
// exercise such a path must run under this helper to mean anything.
func UseAppRoleDB(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, dsn)

	require.NoError(t, models.DB.Exec(`
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
				CREATE ROLE `+appRole+` NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD '`+appRole+`';
			END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO `+appRole+`;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+appRole+`;
		GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+appRole+`;
		GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO `+appRole+`;
	`).Error)

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(appRole, appRole)

	appDB, err := gorm.Open(postgres.Open(u.String()), &gorm.Config{})
	require.NoError(t, err)

	superDB := models.DB
	models.DB = appDB
	t.Cleanup(func() {
		models.DB = superDB
		if sqlDB, err := appDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
}
