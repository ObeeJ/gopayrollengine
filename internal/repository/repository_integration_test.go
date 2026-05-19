//go:build integration

package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestMain bootstraps the encryption KEK before any test runs. EncryptedString
// fields (account_number, bank_code) require it; the production binary calls
// InitEncryption() at startup, so the test must too.
func TestMain(m *testing.M) {
	// Dev-only zero key triggers the "insecure dev key" warning path inside
	// InitEncryption. Sufficient for round-trip testing; never for production.
	models.InitEncryption()
	os.Exit(m.Run())
}

// testDB opens a connection to the integration-test database.
// Run with: DATABASE_URL=postgres://... go test -tags=integration ./internal/repository/...
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping repository integration test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	return db
}

// seedOrg inserts (or reuses) an organization row so foreign keys are satisfied.
// Uses raw SQL to avoid coupling the test to the Organization model's exact shape.
func seedOrg(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	require.NoError(t, db.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW()) ON CONFLICT (id) DO NOTHING",
		id, "test org "+id,
	).Error)
}

// TestKoboRoundTrip_Employee proves Salary survives a write/read cycle through
// the BIGINT column without losing the sub-naira precision that float64 would
// silently drop. ₦1500.50 = 150050 kobo and must come back equal.
func TestKoboRoundTrip_Employee(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	emp := models.Employee{
		ID:             "EMP-" + uuid.New().String()[:8],
		OrganizationID: orgID,
		Name:           "Round-Trip Tester",
		Email:          models.EncryptedString("rt-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.Kobo(150050), // ₦1500.50
		IsActive:       true,
	}
	require.NoError(t, db.Create(&emp).Error)

	var got models.Employee
	require.NoError(t, db.First(&got, "id = ?", emp.ID).Error)
	assert.Equal(t, money.Kobo(150050), got.Salary, "salary must round-trip exactly through BIGINT")
}

// TestKoboRoundTrip_LargeValue confirms BIGINT comfortably stores values that
// would overflow int32 — ₦1 trillion = 100,000,000,000,000 kobo.
func TestKoboRoundTrip_LargeValue(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	huge := money.FromNaira(1_000_000_000_000) // ₦1T
	emp := models.Employee{
		ID:             "EMP-" + uuid.New().String()[:8],
		OrganizationID: orgID,
		Name:           "Whale",
		Email:          models.EncryptedString("whale-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("9999999999"),
		BankCode:       models.EncryptedString("058"),
		Salary:         huge,
		IsActive:       true,
	}
	require.NoError(t, db.Create(&emp).Error)

	var got models.Employee
	require.NoError(t, db.First(&got, "id = ?", emp.ID).Error)
	assert.Equal(t, huge, got.Salary)
}

// TestCheckConstraint_RejectsNegativeSalary proves the CHECK (salary >= 0)
// from migration 000005 actually blocks negative writes at the DB level —
// a structural guarantee independent of any Go-side validation.
func TestCheckConstraint_RejectsNegativeSalary(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	emp := models.Employee{
		ID:             "EMP-" + uuid.New().String()[:8],
		OrganizationID: orgID,
		Name:           "Negative Tester",
		Email:          models.EncryptedString("neg-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0000000000"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.Kobo(-1),
		IsActive:       true,
	}
	err := db.Create(&emp).Error
	require.Error(t, err, "DB must reject negative salary via CHECK constraint")
	assert.Contains(t, err.Error(), "employees_salary_nonneg")
}

// TestEmployeeEmail_PerOrgUniqueness locks in Wave 2 #1: the same email is
// allowed across two organisations but not within one. Pre-migration 000007
// a global unique index let tenant A enumerate tenant B's employee directory
// via unique-violation probing.
func TestEmployeeEmail_PerOrgUniqueness(t *testing.T) {
	db := testDB(t)
	orgA := "ORG-" + uuid.New().String()[:8]
	orgB := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgA)
	seedOrg(t, db, orgB)

	email := "shared-" + uuid.New().String()[:8] + "@example.com"

	mk := func(orgID string) *models.Employee {
		return &models.Employee{
			ID:             "EMP-" + uuid.New().String()[:8],
			OrganizationID: orgID,
			Name:           "Shared Email",
			Email:          models.EncryptedString(email),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}
	}

	require.NoError(t, db.Create(mk(orgA)).Error)
	require.NoError(t, db.Create(mk(orgB)).Error, "same email in a different org must be allowed")

	dup := mk(orgA)
	err := db.Create(dup).Error
	require.Error(t, err, "duplicate email within the same org must be rejected by the unique index")
	assert.Contains(t, err.Error(), "idx_employees_org_email_hmac")
}

// TestEmployeeEmail_BlindIndexLookup proves the application can still find an
// employee by email after the column became random-nonce ciphertext. The
// HMAC digest is deterministic so equality on email_hmac works.
func TestEmployeeEmail_BlindIndexLookup(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	email := "lookup-" + uuid.New().String()[:8] + "@example.com"
	created := &models.Employee{
		ID:             "EMP-" + uuid.New().String()[:8],
		OrganizationID: orgID,
		Name:           "Lookup",
		Email:          models.EncryptedString(email),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.FromNaira(1000),
		IsActive:       true,
	}
	require.NoError(t, db.Create(created).Error)

	var found models.Employee
	require.NoError(t, db.Where("organization_id = ? AND email_hmac = ?",
		orgID, models.BlindIndex(email)).First(&found).Error)
	assert.Equal(t, created.ID, found.ID)
	assert.Equal(t, email, string(found.Email),
		"Scan must decrypt back to the original plaintext")
}

// TestEmployeeEmail_StoredAsCiphertext confirms the on-disk value really is
// ciphertext — not the plaintext email — so a database breach doesn't leak
// PII in the clear.
func TestEmployeeEmail_StoredAsCiphertext(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	plaintext := "cipher-" + uuid.New().String()[:8] + "@example.com"
	emp := &models.Employee{
		ID:             "EMP-" + uuid.New().String()[:8],
		OrganizationID: orgID,
		Name:           "Cipher",
		Email:          models.EncryptedString(plaintext),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.FromNaira(1000),
		IsActive:       true,
	}
	require.NoError(t, db.Create(emp).Error)

	var raw string
	require.NoError(t, db.Raw("SELECT email FROM employees WHERE id = ?", emp.ID).Scan(&raw).Error)
	assert.NotEqual(t, plaintext, raw, "email column must not contain plaintext")
	assert.NotEmpty(t, raw, "email column must be populated with ciphertext")
}

// setupRLSTestRole creates a NOSUPERUSER role that the RLS tests pivot into
// via SET LOCAL ROLE. Without this, the test connects as postgres — a
// superuser, which bypasses RLS entirely (FORCE applies only to the table
// owner, not to superusers). Production deployments must use a non-superuser
// role for the same reason; this fixture mirrors that constraint in tests.
func setupRLSTestRole(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'rls_test_user') THEN
				CREATE ROLE rls_test_user NOSUPERUSER NOBYPASSRLS LOGIN;
			END IF;
		END $$;
		GRANT ALL ON ALL TABLES IN SCHEMA public TO rls_test_user;
		GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO rls_test_user;
	`).Error)
}

// TestRLS_BlocksCrossTenantReads is the load-bearing assertion for Wave 2 #4:
// inside WithOrgScope(orgA), a SELECT * FROM employees with no WHERE clause
// must see ONLY orgA's row. This is structural tenant isolation enforced by
// Postgres — a query that "forgets" the org filter yields zero leaks, not the
// entire multi-tenant table.
func TestRLS_BlocksCrossTenantReads(t *testing.T) {
	db := testDB(t)
	models.DB = db // WithOrgScope reads from the package-level handle.

	orgA := "ORG-" + uuid.New().String()[:8]
	orgB := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgA)
	seedOrg(t, db, orgB)

	mk := func(orgID string) *models.Employee {
		return &models.Employee{
			ID:             "EMP-" + uuid.New().String()[:8],
			OrganizationID: orgID,
			Name:           "RLS Tester",
			Email:          models.EncryptedString("rls-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}
	}
	empA := mk(orgA)
	empB := mk(orgB)
	require.NoError(t, db.Create(empA).Error)
	require.NoError(t, db.Create(empB).Error)

	setupRLSTestRole(t, db)

	// Inside orgA's scope: the unfiltered Find must return only orgA's row.
	// SET LOCAL ROLE pivots away from the superuser connection for the
	// duration of the tx so RLS actually applies.
	var seenInA []models.Employee
	require.NoError(t, models.WithOrgScope(context.Background(), orgA, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", []string{empA.ID, empB.ID}).Find(&seenInA).Error
	}))
	require.Len(t, seenInA, 1, "RLS must filter cross-org rows out of scope")
	assert.Equal(t, empA.ID, seenInA[0].ID)

	// Inside orgB's scope: only orgB's row.
	var seenInB []models.Employee
	require.NoError(t, models.WithOrgScope(context.Background(), orgB, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", []string{empA.ID, empB.ID}).Find(&seenInB).Error
	}))
	require.Len(t, seenInB, 1)
	assert.Equal(t, empB.ID, seenInB[0].ID)

	// Outside any scope (session var unset): the permissive bypass lets both
	// rows through, preserving compatibility with existing code paths that
	// haven't been migrated to WithOrgScope yet.
	var bypass []models.Employee
	require.NoError(t, db.Where("id IN ?", []string{empA.ID, empB.ID}).Find(&bypass).Error)
	assert.Len(t, bypass, 2, "queries without scope must still see both rows during the migration period")
}

// TestRLS_BlocksCrossTenantWrites proves the WITH CHECK clause: inside
// orgA's scope you cannot insert a row tagged with orgB. The DB rejects it
// with new row violates row-level security policy. This closes the write-
// side of the tenant invariant — even a code path that misroutes an orgID
// cannot poison another tenant's data.
func TestRLS_BlocksCrossTenantWrites(t *testing.T) {
	db := testDB(t)
	models.DB = db

	orgA := "ORG-" + uuid.New().String()[:8]
	orgB := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgA)
	seedOrg(t, db, orgB)

	setupRLSTestRole(t, db)

	// Inside orgA's scope (and as a non-superuser), attempt to write a row for orgB.
	err := models.WithOrgScope(context.Background(), orgA, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		return tx.Create(&models.Employee{
			ID:             "EMP-" + uuid.New().String()[:8],
			OrganizationID: orgB, // wrong org — RLS WITH CHECK must reject
			Name:           "Cross-tenant smuggler",
			Email:          models.EncryptedString("smuggle-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}).Error
	})
	require.Error(t, err, "WITH CHECK must reject writes to another tenant's org_id")
	assert.Contains(t, err.Error(), "row-level security")
}

// TestPayrollTotalSum_BigintAggregation proves COALESCE(SUM(total_amount))
// scans cleanly into money.Kobo via the Scan implementation — this is the
// path the compliance report uses.
func TestPayrollTotalSum_BigintAggregation(t *testing.T) {
	db := testDB(t)
	orgID := "ORG-" + uuid.New().String()[:8]
	seedOrg(t, db, orgID)

	// Two completed payrolls totalling ₦450,000.75
	for i, kobo := range []money.Kobo{money.Kobo(20000050), money.Kobo(24999975)} {
		p := models.Payroll{
			ID:             "PAY-" + uuid.New().String()[:8],
			OrganizationID: orgID,
			Period:         "agg-test-" + uuid.New().String()[:8],
			TotalAmount:    kobo,
			Status:         models.PayrollCompleted,
			CreatedAt:      time.Now().Add(time.Duration(-i) * time.Hour),
			UpdatedAt:      time.Now(),
		}
		require.NoError(t, db.Create(&p).Error)
	}

	var total money.Kobo
	require.NoError(t, db.Model(&models.Payroll{}).
		Where("organization_id = ? AND status = ?", orgID, models.PayrollCompleted).
		Select("COALESCE(SUM(total_amount), 0)").
		Scan(&total).Error)

	assert.Equal(t, money.Kobo(45000025), total, "SUM(BIGINT) must scan to Kobo without loss")
}
