package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// D2CHandler — direct-to-consumer worker-facing endpoints. Signup is the
// only handler in this package that creates an Organization rather than
// assuming one already exists, since a D2C worker has no employer to have
// created one for them.
type D2CHandler struct{}

// Signup — POST /api/v1/d2c/signup; public, no auth required (there is no
// identity yet to authenticate). Creates a single-employee D2C organization
// for the worker themself (see Organization.IsD2C's doc comment for why
// that shape is reused rather than a parallel identity system), records
// their acceptance of the D2C repayment disclosure, and returns a worker
// JWT so they can proceed straight to linking a bank account. There is no
// existing employer or OTP identity for a first-time signup to authenticate
// against, so this issues the token directly rather than routing through
// /worker/auth/login.
func (h *D2CHandler) Signup(c *gin.Context) {
	var req struct {
		Name          string         `json:"name" binding:"required"`
		Phone         string         `json:"phone" binding:"required"`
		Email         string         `json:"email" binding:"required,email"`
		AccountNumber string         `json:"account_number" binding:"required"`
		BankCode      string         `json:"bank_code" binding:"required"`
		BVN           string         `json:"bvn"`
		Currency      money.Currency `json:"currency"`

		// AcceptedDisclosure must be explicitly true — the worker has to
		// affirmatively acknowledge the direct-debit repayment mechanism
		// (see docs/EWA_ROADMAP.md §7's unresolved regulatory note: this is
		// real recourse against the worker's own bank account, not payroll
		// deduction) before an account exists, not discover it later at the
		// bank-linking step.
		AcceptedDisclosure bool `json:"accepted_disclosure" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	currency := req.Currency
	if currency == "" {
		currency = money.NGN
	}
	if !currency.IsValid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid currency"})
		return
	}

	// Same CBN KYC rule CreateEmployee applies to an employer-created
	// employee — the obligation is driven by the org's operating currency,
	// not by whether an employer is involved in onboarding.
	requiresBVN := currency == money.NGN
	if requiresBVN && req.BVN == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bvn is required"})
		return
	}

	org := models.Organization{Name: req.Name, IsD2C: true, Currency: currency}
	randomPW, err := randomHex(32)
	if err != nil {
		middleware.Logger.Error("d2c signup: password generation failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
		return
	}
	// A D2C org has no employer login — /auth/login is never a valid path
	// for it — so this hash only needs to make that path unforgeable, never
	// to be typed in by anyone.
	if err := org.SetPassword(randomPW); err != nil {
		middleware.Logger.Error("d2c signup: password hash failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create account"})
		return
	}

	var emp models.Employee
	var user models.User
	expires := time.Now().AddDate(1, 0, 0)
	txErr := models.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		// The org row must exist, in this same transaction, before
		// anything referencing organization_id can be inserted — RLS is
		// scoped by app.org_id below, but the foreign key itself is what
		// actually requires this ordering.
		if err := tx.Create(&org).Error; err != nil {
			return err
		}
		if err := tx.Exec("SELECT set_config('app.org_id', ?, true)", org.ID).Error; err != nil {
			return err
		}

		emp = models.Employee{
			OrganizationID: org.ID,
			Name:           req.Name,
			Email:          models.EncryptedString(req.Email),
			AccountNumber:  models.EncryptedString(req.AccountNumber),
			BankCode:       models.EncryptedString(req.BankCode),
		}
		if err := tx.Create(&emp).Error; err != nil {
			return err
		}

		user = models.User{EmployeeID: emp.ID, OrgID: org.ID, Phone: req.Phone}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}

		if err := tx.Create(&models.ConsentRecord{
			OrganizationID: org.ID,
			EmployeeID:     emp.ID,
			ConsentType:    "d2c_direct_debit_disclosure",
			Granted:        true,
			IPAddress:      c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			ConsentedAt:    time.Now(),
			ExpiresAt:      &expires,
		}).Error; err != nil {
			return err
		}

		return models.AppendAuditTx(tx, org.ID, "Organization", org.ID, "d2c_signup", "", org.Name, c.ClientIP(), "")
	})
	if txErr != nil {
		middleware.Logger.Error("d2c signup transaction failed", "error", txErr.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sign up"})
		return
	}

	// BVN check enqueued async, same as CreateEmployee's own posture —
	// Dojah latency and transient failures don't block the response.
	if requiresBVN {
		if err := workers.EnqueueBVNVerification(org.ID, emp.ID, req.BVN); err != nil {
			middleware.Logger.Warn("d2c signup: BVN enqueue failed", "employee_id", emp.ID, "error", err.Error())
		}
	}

	token, err := middleware.IssueWorkerToken(org.ID, emp.ID, 8*time.Hour)
	if err != nil {
		middleware.Logger.Error("d2c signup: token issue failed", "org_id", org.ID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account created but could not issue token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"token":       token,
		"expires_in":  "8h",
		"org_id":      org.ID,
		"employee_id": emp.ID,
		"role":        "employee",
	})
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
