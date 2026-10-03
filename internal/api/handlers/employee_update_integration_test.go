//go:build integration

package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func employeeRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.OrgIDKey, c.GetHeader("X-Org"))
		c.Set("role", "admin")
		c.Next()
	})
	r.PATCH("/employees/:id", newEmployeeHandlerForTest().UpdateEmployee)
	return r
}

func patch(t *testing.T, orgID, empID, body string) (int, map[string]any) {
	t.Helper()
	return call(t, employeeRouter(), http.MethodPatch, "/employees/"+empID, orgID, body)
}

func reloadEmployee(t *testing.T, id string) models.Employee {
	t.Helper()
	var e models.Employee
	require.NoError(t, models.DB.First(&e, "id = ?", id).Error)
	return e
}

func auditsFor(t *testing.T, orgID, entityID, action string) []models.AuditEvent {
	t.Helper()
	var out []models.AuditEvent
	require.NoError(t, models.DB.Where("organization_id = ? AND entity_id = ? AND action = ?", orgID, entityID, action).
		Find(&out).Error)
	return out
}

func TestUpdateEmployee_ChangesOnlyWhatWasSent(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	id := seedActiveSalaried(t, orgID, "Ada", money.FromNaira(300_000))
	before := reloadEmployee(t, id)

	code, out := patch(t, orgID, id, `{"name":"Ada Obi"}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, "Ada Obi", out["name"])
	after := reloadEmployee(t, id)
	assert.Equal(t, "Ada Obi", after.Name)
	assert.Equal(t, before.Salary, after.Salary)
	assert.Equal(t, before.AccountNumber, after.AccountNumber)
	assert.True(t, after.IsActive)
}

func TestUpdateEmployee_BankDetailsAreValidatedAuditedAndMasked(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	id := seedActiveSalaried(t, orgID, "Emeka", money.FromNaira(180_000)) // account 0123456789, bank 058

	code, out := patch(t, orgID, id, `{"account_number":"0987654321","bank_code":"011"}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	e := reloadEmployee(t, id)
	assert.Equal(t, "0987654321", e.AccountNumber.String(), "stored encrypted, readable through the model")
	assert.Equal(t, "011", e.BankCode.String())
	assert.Equal(t, "****4321", out["account_number"], "the response masks it")

	audits := auditsFor(t, orgID, id, "updated")
	require.Len(t, audits, 1)
	text := audits[0].Before + " | " + audits[0].After
	assert.Contains(t, text, "account_number")
	assert.NotContains(t, text, "0987654321", "the audit log must not hold the full new account number")
	assert.NotContains(t, text, "0123456789", "…nor the old one")
	assert.Contains(t, text, "6789")
	assert.Contains(t, text, "4321")
	assert.Equal(t, "admin", audits[0].ActorKey)
}

func TestUpdateEmployee_Validation(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	salaried := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	hourly := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Employee{
		ID: hourly, OrganizationID: orgID, Name: "Chioma", Email: models.EncryptedString("chioma-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber: "0234567891", BankCode: "058", WageType: models.WageHourly,
		HourlyRateKobo: money.FromNaira(1_500), IsActive: true,
	}).Error)

	for name, tc := range map[string]struct {
		id, body, field string
	}{
		"empty body":                 {salaried, `{}`, ""},
		"unknown field":              {salaried, `{"is_active":false}`, ""},
		"9-digit account":            {salaried, `{"account_number":"012345678"}`, "account_number"},
		"letters in account":         {salaried, `{"account_number":"01234567AB"}`, "account_number"},
		"non-numeric bank code":      {salaried, `{"bank_code":"GTB"}`, "bank_code"},
		"zero salary":                {salaried, `{"salary":0}`, "salary"},
		"negative salary":            {salaried, `{"salary":-5}`, "salary"},
		"salary on an hourly worker": {hourly, `{"salary":30000000}`, "salary"},
		"rate on a salaried worker":  {salaried, `{"hourly_rate_kobo":150000}`, "hourly_rate_kobo"},
		"bad email":                  {salaried, `{"email":"not-an-email"}`, "email"},
		"local-format phone":         {salaried, `{"phone":"08031234567"}`, "phone"},
		"blank name":                 {salaried, `{"name":"   "}`, "name"},
		"malformed JSON":             {salaried, `{`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := patch(t, orgID, tc.id, tc.body)
			assert.Equal(t, http.StatusBadRequest, code, "%v", out)
			if tc.field != "" {
				fields, _ := out["fields"].(map[string]any)
				assert.Contains(t, fields, tc.field, "%v", out)
			}
		})
	}
	assert.Equal(t, money.FromNaira(300_000), reloadEmployee(t, salaried).Salary, "a rejected update changes nothing")
}

func TestUpdateEmployee_SalaryAndHourlyRate(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	id := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	code, out := patch(t, orgID, id, `{"salary":35000000}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, money.FromNaira(350_000), reloadEmployee(t, id).Salary)
	audits := auditsFor(t, orgID, id, "updated")
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].Before+audits[0].After, "salary")
}

func TestUpdateEmployee_ConflictsAre409(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	a := seedActiveSalaried(t, orgID, "Ada", money.FromNaira(100_000))
	b := seedActiveSalaried(t, orgID, "Bola", money.FromNaira(100_000))
	emailA := reloadEmployee(t, a).Email.String()

	code, out := patch(t, orgID, b, `{"email":"`+emailA+`"}`)
	assert.Equal(t, http.StatusConflict, code, "an email already used in this company: %v", out)

	phone := testPhone()
	require.NoError(t, models.DB.Create(&models.User{EmployeeID: a, OrgID: orgID, Phone: phone}).Error)
	code, out = patch(t, orgID, b, `{"phone":"`+phone+`"}`)
	assert.Equal(t, http.StatusConflict, code, "a phone already registered to a worker: %v", out)
}

func TestUpdateEmployee_PhoneCreatesOrMovesTheWorkerLogin(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	id := seedActiveSalaried(t, orgID, "Ada", money.FromNaira(100_000))
	first, second := testPhone(), testPhone()

	code, out := patch(t, orgID, id, `{"phone":"`+first+`"}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	var u models.User
	require.NoError(t, models.DB.First(&u, "employee_id = ?", id).Error)
	assert.Equal(t, first, u.Phone, "an employee hired without a phone gains a login")
	assert.True(t, u.IsActive)

	code, _ = patch(t, orgID, id, `{"phone":"`+second+`"}`)
	require.Equal(t, http.StatusOK, code)
	var count int64
	require.NoError(t, models.DB.Model(&models.User{}).Where("employee_id = ?", id).Count(&count).Error)
	assert.EqualValues(t, 1, count, "moving the phone must not create a second login")
	require.NoError(t, models.DB.First(&u, "employee_id = ?", id).Error)
	assert.Equal(t, second, u.Phone)
	require.Error(t, models.DB.First(&models.User{}, "phone = ?", first).Error, "the old number can no longer log in")
}

func TestUpdateEmployee_TerminatedAndOtherTenantAndNoOp(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	id := seedActiveSalaried(t, orgID, "Ada", money.FromNaira(100_000))

	t.Run("a no-op succeeds and leaves no audit row", func(t *testing.T) {
		code, _ := patch(t, orgID, id, `{"name":"Ada"}`)
		assert.Equal(t, http.StatusOK, code)
		assert.Empty(t, auditsFor(t, orgID, id, "updated"))
	})
	t.Run("another tenant sees no such employee", func(t *testing.T) {
		other := seedOrgWithCurrency(t, money.NGN)
		testutil.UseAppRoleDB(t)
		code, _ := patch(t, other, id, `{"name":"Hijacked"}`)
		assert.Equal(t, http.StatusNotFound, code)
	})
	// Back on the superuser connection (the subtest's cleanup restored it).
	assert.Equal(t, "Ada", reloadEmployee(t, id).Name, "the other tenant's request changed nothing")
	t.Run("a terminated employee cannot be edited", func(t *testing.T) {
		_, err := services.NewEmployeeTerminationService().Terminate(context.Background(), orgID, id, "left", "127.0.0.1")
		require.NoError(t, err)
		code, _ := patch(t, orgID, id, `{"name":"Zombie"}`)
		assert.Equal(t, http.StatusConflict, code)
	})
	t.Run("unknown ID", func(t *testing.T) {
		code, _ := patch(t, orgID, "EMP-nosuch00", `{"name":"x"}`)
		assert.Equal(t, http.StatusNotFound, code)
	})
}

// The scenario this exists for: a payment bounces because the account is
// closed, the admin corrects it, retries — and the retry must reach the bank
// with the NEW account number, not the one that failed.
func TestUpdateEmployee_RetryAfterFixingTheAccountPaysTheNewAccount(t *testing.T) {
	skipIfNoDB(t)
	bank := newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)
	require.Equal(t, "0123456789", bank.accountFor(emeka.ID), "attempt 1 went to the account that has since been closed")

	code, out := patch(t, orgID, emeka.EmployeeID, `{"account_number":"0555666777","bank_code":"033"}`)
	require.Equal(t, http.StatusOK, code, "%v", out)

	_, err := newPayrollSvc().RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	runWorker(t, orgID, payrollID)
	assert.Equal(t, "0555666777", bank.accountFor(emeka.ID+"-R2"), "the retry must pay the corrected account")
}

// UpdateEmployee loads the row and saves it back whole. Without a row lock, an
// edit that loaded the employee while still active would, if a termination
// committed in between, write is_active=true back: a terminated employee
// resurrected by an unrelated rename, who could then be paid.
func TestUpdateEmployee_ConcurrentTerminationIsNeverOverwritten(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	svc := services.NewEmployeeService()

	for i := 0; i < 25; i++ {
		id := seedActiveSalaried(t, orgID, "Racer", money.FromNaira(100_000))
		name := "Renamed"
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = services.NewEmployeeTerminationService().Terminate(context.Background(), orgID, id, "left", "127.0.0.1")
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.UpdateEmployee(context.Background(), orgID, id, services.EmployeeUpdate{Name: &name}, services.Actor{Name: "admin"})
		}()
		wg.Wait()
		require.False(t, reloadEmployee(t, id).IsActive, "iteration %d: a terminated employee was resurrected by a concurrent edit", i)
	}
}
