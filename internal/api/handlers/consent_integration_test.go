//go:build integration

package handlers

import (
	"context"
	"net/http"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func consentCount(t *testing.T, orgID, employeeID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Model(&models.ConsentRecord{}).Where("employee_id = ?", employeeID).Count(&n).Error
	}))
	return n
}

// The foreign key on consent_records.employee_id is checked by Postgres with
// row-level security bypassed, so an INSERT naming another tenant's employee
// passes it, and RLS's WITH CHECK only looks at organization_id (the caller's
// own). The handler must therefore verify the employee belongs to the caller's
// org itself. Found by the live end-to-end run against the production DB role:
// Kano's admin recorded "marketing consent withdrawn" for Swift's Musa.
func TestRecordConsent_RefusesAnotherTenantsEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgA := seedOrgWithCurrency(t, money.NGN)
	orgB := seedOrgWithCurrency(t, money.NGN)
	victim := seedActiveSalaried(t, orgA, "Musa", money.FromNaira(300_000))
	h := (&ConsentHandler{}).RecordConsent

	run := func(t *testing.T) {
		w, c := newEmployeeCreateRequest(t, orgB, map[string]any{
			"employee_id": victim, "consent_type": "marketing", "granted": false,
		})
		h(c)
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		assert.Zero(t, consentCount(t, orgA, victim), "no row may be written against the other tenant's employee")
	}
	t.Run("superuser connection", run)
	t.Run("production role (RLS enforced)", func(t *testing.T) {
		testutil.UseAppRoleDB(t)
		run(t)
	})
}

// An unknown employee ID used to surface as a foreign-key violation and a 500,
// which also made the endpoint an oracle for which employee IDs exist.
func TestRecordConsent_UnknownEmployeeIs404NotAServerError(t *testing.T) {
	skipIfNoDB(t)
	org := seedOrgWithCurrency(t, money.NGN)
	w, c := newEmployeeCreateRequest(t, org, map[string]any{
		"employee_id": "EMP-nosuch00", "consent_type": "marketing", "granted": true,
	})
	(&ConsentHandler{}).RecordConsent(c)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "constraint", "internals must not leak")
}

// The other tenant and the nonexistent case must be indistinguishable.
func TestRecordConsent_OtherTenantAndUnknownLookIdentical(t *testing.T) {
	skipIfNoDB(t)
	orgA := seedOrgWithCurrency(t, money.NGN)
	orgB := seedOrgWithCurrency(t, money.NGN)
	victim := seedActiveSalaried(t, orgA, "Musa", money.FromNaira(300_000))
	h := (&ConsentHandler{}).RecordConsent

	w1, c1 := newEmployeeCreateRequest(t, orgB, map[string]any{"employee_id": victim, "consent_type": "x", "granted": true})
	h(c1)
	w2, c2 := newEmployeeCreateRequest(t, orgB, map[string]any{"employee_id": "EMP-nosuch00", "consent_type": "x", "granted": true})
	h(c2)
	assert.Equal(t, w1.Code, w2.Code)
	assert.JSONEq(t, w1.Body.String(), w2.Body.String())
}

func TestRecordConsent_OwnEmployeeSucceedsAndIsAudited(t *testing.T) {
	skipIfNoDB(t)
	org := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, org, "Chioma", money.FromNaira(150_000))
	w, c := newEmployeeCreateRequest(t, org, map[string]any{
		"employee_id": emp, "consent_type": "marketing", "granted": false,
	})
	(&ConsentHandler{}).RecordConsent(c)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.EqualValues(t, 1, consentCount(t, org, emp))

	var audits int64
	require.NoError(t, models.DB.Model(&models.AuditEvent{}).
		Where("organization_id = ? AND action = ?", org, "consent_recorded").Count(&audits).Error)
	assert.EqualValues(t, 1, audits)
}

// Withdrawing consent after leaving the company is a legitimate NDPR request.
func TestRecordConsent_TerminatedEmployeeCanStillWithdraw(t *testing.T) {
	skipIfNoDB(t)
	org := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, org, "Emeka", money.FromNaira(180_000))
	require.NoError(t, models.DB.Model(&models.Employee{}).Where("id = ?", emp).Update("is_active", false).Error)
	w, c := newEmployeeCreateRequest(t, org, map[string]any{
		"employee_id": emp, "consent_type": "payroll_processing", "granted": false,
	})
	(&ConsentHandler{}).RecordConsent(c)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}
