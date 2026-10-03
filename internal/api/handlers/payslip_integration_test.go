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
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func payslipsFor(t *testing.T, orgID, employeeID string) []services.Payslip {
	t.Helper()
	got, _, err := newPayrollSvc().ListPayslips(context.Background(), orgID, employeeID, 1, 50)
	require.NoError(t, err)
	return got
}

func TestPayslips_ShowTheBreakdownAndFollowThePaymentToPaid(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	require.NoError(t, models.DB.Create(&models.EWASavingsPreference{
		OrganizationID: orgID, EmployeeID: emp, Enabled: true, Mode: models.SavingsFixedPercent, FixedPercent: 5,
	}).Error)
	payrollID := createPayroll(t, orgID)
	runWorker(t, orgID, payrollID)

	slips := payslipsFor(t, orgID, emp)
	require.Len(t, slips, 1)
	s := slips[0]
	assert.Equal(t, time.Now().Format(services.PeriodLayout), s.Period)
	assert.Equal(t, services.PayslipOnItsWay, s.State, "handed to the bank, not yet confirmed")
	assert.Equal(t, money.FromNaira(285_000), s.Net)
	require.True(t, s.BreakdownAvailable)
	assert.Equal(t, money.FromNaira(300_000), *s.Gross)
	assert.Equal(t, money.FromNaira(15_000), *s.Savings)
	assert.Zero(t, *s.AdvancesDeducted)
	assert.Nil(t, s.PaidAt)

	it := itemByName(t, payrollID, "Musa")
	require.Equal(t, http.StatusOK, deliver(t, it.ID, "DISBURSEMENT_SUCCESSFUL", it.Amount))
	s = payslipsFor(t, orgID, emp)[0]
	assert.Equal(t, services.PayslipPaid, s.State)
	require.NotNil(t, s.PaidAt, "the date the money landed")
}

func TestPayslips_AFailedPaymentIsVisibleToTheWorker(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)

	s := payslipsFor(t, orgID, emeka.EmployeeID)[0]
	assert.Equal(t, services.PayslipFailed, s.State, "a bounced payment must not look like a payment in progress")

	_, err := newPayrollSvc().RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	runWorker(t, orgID, payrollID)
	assert.Equal(t, services.PayslipOnItsWay, payslipsFor(t, orgID, emeka.EmployeeID)[0].State)
}

func TestPayslips_OnlyTheWorkersOwnLines(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	musa := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	chioma := seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	runWorker(t, orgID, createPayroll(t, orgID))

	mine := payslipsFor(t, orgID, musa)
	require.Len(t, mine, 1)
	assert.Equal(t, money.FromNaira(300_000), mine[0].Net)
	theirs := payslipsFor(t, orgID, chioma)
	require.Len(t, theirs, 1)
	assert.Equal(t, money.FromNaira(150_000), theirs[0].Net)

	t.Run("another tenant, production role", func(t *testing.T) {
		other := seedOrgWithCurrency(t, money.NGN)
		testutil.UseAppRoleDB(t)
		assert.Empty(t, payslipsFor(t, other, musa), "knowing an employee ID from another company reveals nothing")
	})
}

// Items created before the breakdown was recorded have only a net amount. The
// payslip must say so rather than invent a gross.
func TestPayslips_OldItemsHaveNoBreakdownAndSayNoGross(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	payroll := models.Payroll{OrganizationID: orgID, Period: "2026-01", Status: models.PayrollCompleted}
	require.NoError(t, models.DB.Create(&payroll).Error)
	require.NoError(t, models.DB.Create(&models.PayrollItem{
		OrganizationID: orgID, PayrollID: payroll.ID, EmployeeID: emp, EmployeeName: "Musa",
		Amount: money.FromNaira(250_000), Status: models.PayrollCompleted,
	}).Error)

	s := payslipsFor(t, orgID, emp)[0]
	assert.False(t, s.BreakdownAvailable)
	assert.Nil(t, s.Gross)
	assert.Equal(t, money.FromNaira(250_000), s.Net)
	assert.Equal(t, services.PayslipPaid, s.State)

	raw, err := json.Marshal(s)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"gross"`, "an absent figure is omitted, not zero")
}

func TestPayslips_NewestFirstAndPaginated(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	for _, period := range []string{"2026-05", "2026-06", "2026-07"} {
		payroll := models.Payroll{OrganizationID: orgID, Period: period, Status: models.PayrollCompleted}
		require.NoError(t, models.DB.Create(&payroll).Error)
		require.NoError(t, models.DB.Create(&models.PayrollItem{
			OrganizationID: orgID, PayrollID: payroll.ID, EmployeeID: emp, EmployeeName: "Musa",
			Amount: money.FromNaira(1), Status: models.PayrollCompleted,
		}).Error)
		time.Sleep(10 * time.Millisecond)
	}
	got, total, err := newPayrollSvc().ListPayslips(context.Background(), orgID, emp, 1, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	require.Len(t, got, 2)
	assert.Equal(t, "2026-07", got[0].Period)
	assert.Equal(t, "2026-06", got[1].Period)
	page2, _, err := newPayrollSvc().ListPayslips(context.Background(), orgID, emp, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, "2026-05", page2[0].Period)
}

func TestPayslips_HTTPUsesTheCallersOwnIdentity(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	musa := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	runWorker(t, orgID, createPayroll(t, orgID))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.OrgIDKey, orgID)
		c.Set("employee_id", musa) // as RequireWorker + JWT would set it
		c.Next()
	})
	r.GET("/worker/payslips", NewPayslipHandler(newPayrollSvc()).ListPayslips)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/worker/payslips?page_size=500", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var out struct {
		Data     []map[string]any `json:"data"`
		Total    int              `json:"total"`
		PageSize int              `json:"page_size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, 1, out.Total, "only Musa's line, never Chioma's")
	require.Len(t, out.Data, 1)
	assert.EqualValues(t, 30000000, out.Data[0]["net"])
	assert.Equal(t, 20, out.PageSize, "an oversized page falls back to the default")
	for _, internal := range []string{"resolved_by", "resolution_note", "resolution_evidence", "attempt", "error_message", "organization_id", "employee_id"} {
		assert.NotContains(t, out.Data[0], internal, "internal field %q must not reach a worker", internal)
	}
}
