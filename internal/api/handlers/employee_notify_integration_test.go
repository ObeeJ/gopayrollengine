//go:build integration

package handlers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sentText struct{ phone, body string }

type recordingTexts struct {
	mu   sync.Mutex
	sent []sentText
	err  error
}

func (r *recordingTexts) SendText(_ context.Context, phone, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentText{phone, text})
	return r.err
}

func (r *recordingTexts) all() []sentText {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentText(nil), r.sent...)
}

func strp(s string) *string { return &s }

func workerWithLogin(t *testing.T) (orgID, empID, phone string) {
	t.Helper()
	orgID = seedOrgWithCurrency(t, money.NGN)
	empID = seedActiveSalaried(t, orgID, "Emeka", money.FromNaira(180_000))
	phone = testPhone()
	require.NoError(t, models.DB.Create(&models.User{EmployeeID: empID, OrgID: orgID, Phone: phone}).Error)
	return
}

func TestEmployeeUpdate_BankChangeTextsTheWorker(t *testing.T) {
	skipIfNoDB(t)
	orgID, empID, phone := workerWithLogin(t)
	rec := &recordingTexts{}
	svc := &services.EmployeeService{Notifier: rec}

	_, err := svc.UpdateEmployee(context.Background(), orgID, empID,
		services.EmployeeUpdate{AccountNumber: strp("0987654321")}, services.Actor{Name: "admin"})
	require.NoError(t, err)
	svc.WaitNotifications()

	got := rec.all()
	require.Len(t, got, 1)
	assert.Equal(t, phone, got[0].phone, "goes to the worker's login phone")
	assert.Contains(t, got[0].body, "4321", "names the account by its last four digits")
	assert.NotContains(t, got[0].body, "0987654321", "never the full account number")
	assert.Contains(t, got[0].body, "contact your employer")
}

func TestEmployeeUpdate_NoTextWhenNothingAboutPayChanges(t *testing.T) {
	skipIfNoDB(t)
	orgID, empID, _ := workerWithLogin(t)
	rec := &recordingTexts{}
	svc := &services.EmployeeService{Notifier: rec}
	act := services.Actor{Name: "admin"}

	_, err := svc.UpdateEmployee(context.Background(), orgID, empID, services.EmployeeUpdate{Name: strp("Emeka O.")}, act)
	require.NoError(t, err)
	// same account number as stored: not a change
	_, err = svc.UpdateEmployee(context.Background(), orgID, empID, services.EmployeeUpdate{AccountNumber: strp("0123456789")}, act)
	require.NoError(t, err)
	// invalid details: rolled back, nothing sent
	_, err = svc.UpdateEmployee(context.Background(), orgID, empID, services.EmployeeUpdate{AccountNumber: strp("12")}, act)
	require.Error(t, err)
	svc.WaitNotifications()

	assert.Empty(t, rec.all())
}

func TestEmployeeUpdate_PhoneChangeWarnsTheOldNumber(t *testing.T) {
	skipIfNoDB(t)
	orgID, empID, oldPhone := workerWithLogin(t)
	rec := &recordingTexts{}
	svc := &services.EmployeeService{Notifier: rec}

	newPhone := testPhone()
	_, err := svc.UpdateEmployee(context.Background(), orgID, empID, services.EmployeeUpdate{Phone: &newPhone}, services.Actor{Name: "admin"})
	require.NoError(t, err)
	svc.WaitNotifications()

	got := rec.all()
	require.Len(t, got, 1)
	assert.Equal(t, oldPhone, got[0].phone, "the previous owner of the number is the one who needs to know")
	assert.True(t, strings.Contains(got[0].body, "phone number"))
}

func TestEmployeeUpdate_FailedTextNeverFailsTheUpdate(t *testing.T) {
	skipIfNoDB(t)
	orgID, empID, _ := workerWithLogin(t)
	svc := &services.EmployeeService{Notifier: &recordingTexts{err: errors.New("gateway down")}}

	_, err := svc.UpdateEmployee(context.Background(), orgID, empID,
		services.EmployeeUpdate{BankCode: strp("011")}, services.Actor{Name: "admin"})
	require.NoError(t, err)
	svc.WaitNotifications()

	assert.Equal(t, "011", reloadEmployee(t, empID).BankCode.String(), "the change stands")
}

func TestEmployeeUpdate_NoNotifierMeansNoTextAndNoPanic(t *testing.T) {
	skipIfNoDB(t)
	orgID, empID, _ := workerWithLogin(t)
	_, err := services.NewEmployeeService().UpdateEmployee(context.Background(), orgID, empID,
		services.EmployeeUpdate{BankCode: strp("011")}, services.Actor{Name: "admin"})
	require.NoError(t, err)
}
