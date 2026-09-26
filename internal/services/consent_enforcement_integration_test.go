//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// createConsent writes one ConsentRecord row directly, the same way
// D2CBankLinkHandler and ConsentHandler do — a helper here rather than
// spelling out WithOrgScope + tx.Create at every call site below.
func createConsent(t *testing.T, orgID, employeeID, consentType string, granted bool, consentedAt time.Time) {
	t.Helper()
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    consentType,
			Granted:        granted,
			ConsentedAt:    consentedAt,
		}).Error
	}))
}

// No ConsentRecord has ever been written for this (org, employee, type) —
// the ordinary state before anyone has ever consented to anything.
// HasActiveConsent must fail closed, not treat "no row" as implicit grant.
func TestHasActiveConsent_NoRecordIsNotActive(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)

	assert.False(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CBankLinkRead))
}

func TestHasActiveConsent_GrantedAndNotExpiredIsActive(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, true, time.Now())

	assert.True(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CBankLinkRead))
}

// ConsentRecord is append-only — a withdrawal is a brand new row with
// Granted: false, the original granted row is never touched — so this is
// the exact case the previous implementation got wrong: it counted any
// matching granted=true row and found the original grant still sitting
// there, reporting active consent forever regardless of a later
// withdrawal. Only the most recent row must decide the current state.
func TestHasActiveConsent_LaterWithdrawalOverridesEarlierGrant(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	base := time.Now()
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, true, base)
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, false, base.Add(time.Second))

	assert.False(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CBankLinkRead))
}

// The mirror image: a later re-grant must override an earlier withdrawal —
// consent isn't gone forever just because it was once withdrawn.
func TestHasActiveConsent_LaterGrantOverridesEarlierWithdrawal(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	base := time.Now()
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, false, base)
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, true, base.Add(time.Second))

	assert.True(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CBankLinkRead))
}

func TestHasActiveConsent_ExpiredGrantIsNotActive(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	expired := time.Now().Add(-time.Hour)
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    models.ConsentTypeD2CDebitMandate,
			Granted:        true,
			ConsentedAt:    time.Now().Add(-2 * time.Hour),
			ExpiresAt:      &expired,
		}).Error
	}))

	assert.False(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CDebitMandate))
}

// A different consent type for the same worker must not leak into this
// one — withdrawing bank-link read consent must never look like it also
// withdrew the (entirely separate) debit-mandate consent.
func TestHasActiveConsent_DoesNotLeakAcrossConsentTypes(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CBankLinkRead, false, time.Now())
	createConsent(t, orgID, employeeID, models.ConsentTypeD2CDebitMandate, true, time.Now())

	assert.False(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CBankLinkRead))
	assert.True(t, models.HasActiveConsent(models.DB, orgID, employeeID, models.ConsentTypeD2CDebitMandate))
}
