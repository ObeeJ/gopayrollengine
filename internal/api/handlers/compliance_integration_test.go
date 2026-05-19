//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupRLSRole creates a NOSUPERUSER role that the RLS tests pivot into
// via SET LOCAL ROLE. Production deployments connect as payroll_app (also
// NOSUPERUSER) — this fixture exercises the same RLS code path the live
// system runs under. Idempotent so suite re-runs work.
func setupRLSRole(t *testing.T) {
	t.Helper()
	require.NoError(t, models.DB.Exec(`
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

// seedComplianceFixtures creates two orgs with disjoint payroll, consent and
// audit data so a cross-tenant read would be immediately visible in the
// compliance report's aggregate counts.
type complianceFixtures struct {
	orgA, orgB             string
	aPayrolls, bPayrolls   int
	aConsents, bConsents   int
}

func seedComplianceFixtures(t *testing.T) complianceFixtures {
	t.Helper()
	f := complianceFixtures{
		orgA:      "ORG-" + uuid.New().String()[:8],
		orgB:      "ORG-" + uuid.New().String()[:8],
		aPayrolls: 3,
		bPayrolls: 7,
		aConsents: 2,
		bConsents: 5,
	}
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		f.orgA, "rls compliance A",
	).Error)
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		f.orgB, "rls compliance B",
	).Error)

	seedPayrolls := func(orgID string, n int) {
		for i := 0; i < n; i++ {
			require.NoError(t, models.DB.Create(&models.Payroll{
				ID:             "PAY-" + uuid.New().String()[:8],
				OrganizationID: orgID,
				Period:         "compliance-" + uuid.New().String()[:8],
				TotalAmount:    money.FromNaira(1000),
				Status:         models.PayrollCompleted,
			}).Error)
		}
	}
	seedPayrolls(f.orgA, f.aPayrolls)
	seedPayrolls(f.orgB, f.bPayrolls)

	// Consent records FK to employees, so seed a real employee per org and
	// hang every consent record off it.
	seedEmployee := func(orgID string) string {
		empID := "EMP-" + uuid.New().String()[:8]
		require.NoError(t, models.DB.Create(&models.Employee{
			ID:             empID,
			OrganizationID: orgID,
			Name:           "Compliance Fixture",
			Email:          models.EncryptedString("comp-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}).Error)
		return empID
	}
	empA := seedEmployee(f.orgA)
	empB := seedEmployee(f.orgB)

	seedConsents := func(orgID, employeeID string, n int) {
		for i := 0; i < n; i++ {
			require.NoError(t, models.DB.Create(&models.ConsentRecord{
				ID:             "CON-" + uuid.New().String()[:8],
				OrganizationID: orgID,
				EmployeeID:     employeeID,
				ConsentType:    "payroll_processing",
				Granted:        true,
				ConsentedAt:    time.Now(),
			}).Error)
		}
	}
	seedConsents(f.orgA, empA, f.aConsents)
	seedConsents(f.orgB, empB, f.bConsents)

	return f
}

// TestCompliance_RLSBlocksCrossTenantAggregates is the load-bearing assertion
// for the compliance_handler RLS migration. The handler's unfiltered queries
// (e.g. tx.Model(&Payroll{}).Count(&n) with no WHERE on org_id) must return
// counts for the caller's org only — enforced by the RLS policy attached in
// migration 000008, not by code-level filters. We exercise the same query
// shape under a non-superuser role to prove the structural guarantee.
func TestCompliance_RLSBlocksCrossTenantAggregates(t *testing.T) {
	skipIfNoDB(t)
	setupRLSRole(t)
	f := seedComplianceFixtures(t)

	since := time.Now().AddDate(0, 0, -30)

	// As orgA, the unfiltered Count must return orgA's row count only.
	var aPayrollCount, aConsentCount int64
	require.NoError(t, models.WithOrgScope(context.Background(), f.orgA, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		if err := tx.Model(&models.Payroll{}).
			Where("created_at >= ?", since).
			Count(&aPayrollCount).Error; err != nil {
			return err
		}
		return tx.Model(&models.ConsentRecord{}).Count(&aConsentCount).Error
	}))
	assert.Equal(t, int64(f.aPayrolls), aPayrollCount,
		"compliance Count() under RLS must see only orgA's payrolls — no cross-tenant leak")
	assert.Equal(t, int64(f.aConsents), aConsentCount,
		"compliance Count() under RLS must see only orgA's consent records")

	// And the inverse: as orgB, only orgB's rows.
	var bPayrollCount int64
	require.NoError(t, models.WithOrgScope(context.Background(), f.orgB, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		return tx.Model(&models.Payroll{}).
			Where("created_at >= ?", since).
			Count(&bPayrollCount).Error
	}))
	assert.Equal(t, int64(f.bPayrolls), bPayrollCount)
}

// TestCompliance_HandlerEndToEnd_RLS exercises the GetComplianceReport HTTP
// handler against a non-superuser connection so the RLS path is fully in
// play. The report's payroll_summary.total_batches must reflect the caller's
// org only, even though the handler's queries no longer carry explicit
// organization_id filters. This is the regression-proof: removing a WHERE
// clause must not become a leak.
func TestCompliance_HandlerEndToEnd_RLS(t *testing.T) {
	skipIfNoDB(t)
	setupRLSRole(t)
	f := seedComplianceFixtures(t)

	// Pivot models.DB to the non-superuser role for the duration of this test
	// so the handler's WithOrgScope path actually engages RLS. Restore at end.
	originalDB := models.DB
	pivotDB := originalDB.Session(&gorm.Session{NewDB: false})
	require.NoError(t, pivotDB.Exec("SET ROLE rls_test_user").Error)
	models.DB = pivotDB
	t.Cleanup(func() {
		_ = pivotDB.Exec("RESET ROLE").Error
		models.DB = originalDB
	})

	h := &ComplianceHandler{}

	for _, tc := range []struct {
		name        string
		orgID       string
		wantPayroll int64
	}{
		{"orgA sees only its 3 payrolls", f.orgA, int64(f.aPayrolls)},
		{"orgB sees only its 7 payrolls", f.orgB, int64(f.bPayrolls)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/compliance/report", nil)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			c.Set(middleware.OrgIDKey, tc.orgID)

			h.GetComplianceReport(c)
			require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

			var got struct {
				OrgID          string `json:"org_id"`
				PayrollSummary struct {
					TotalBatches int64 `json:"total_batches"`
				} `json:"payroll_summary"`
				ConsentSummary struct {
					TotalRecords int64 `json:"total_records"`
				} `json:"consent_summary"`
				SecurityChecks map[string]bool `json:"security_checks"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			assert.Equal(t, tc.orgID, got.OrgID)
			assert.Equal(t, tc.wantPayroll, got.PayrollSummary.TotalBatches,
				"handler must report only the caller's org payroll count under RLS")
			assert.True(t, got.SecurityChecks["rls_enforced"],
				"compliance report must advertise that RLS is in force")
		})
	}
}
