//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/testutil"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Run as a production-shaped (non-superuser, RLS-enforced) role:
// bvn_verifications has forced row-level security, so a write outside an org
// scope is rejected by its WITH CHECK policy. The superuser this suite
// otherwise connects as bypasses RLS entirely, which is how an unscoped
// insert here passed every test while failing — every retry, every time — in
// production, leaving no BVN/KYC result recorded for anyone.
func TestProcessBVNTask_PersistsUnderProductionRole(t *testing.T) {
	skipIfNoDB(t)
	t.Setenv("MOCK_MODE", "true")

	orgID := "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "bvn test org",
	).Error)
	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Kemi",
		Email:          models.EncryptedString("kemi-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		IsActive:       true,
	}).Error)

	testutil.UseAppRoleDB(t)

	payload, err := json.Marshal(map[string]string{
		"org_id": orgID, "employee_id": employeeID, "bvn": "12345678901",
	})
	require.NoError(t, err)

	err = NewBVNHandler().ProcessBVNTask(context.Background(), asynq.NewTask(TypeVerifyBVN, payload))
	require.NoError(t, err, "BVN result must persist under the production database role")

	var count int64
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Model(&models.BVNVerification{}).Where("employee_id = ?", employeeID).Count(&count).Error
	}))
	assert.Equal(t, int64(1), count)
}
