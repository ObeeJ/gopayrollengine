package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// pgUniqueViolation is Postgres's own SQLSTATE for a unique constraint
// violation (23505) — https://www.postgresql.org/docs/current/errcodes-appendix.html.
const pgUniqueViolation = "23505"

// D2CHandler — direct-to-consumer worker-facing endpoints. Signup is the
// only handler in this package that creates an Organization rather than
// assuming one already exists, since a D2C worker has no employer to have
// created one for them.
type D2CHandler struct {
	// BankLinkUnavailable — true when routes.go has no real banklink
	// provider to wire up (i.e. outside MOCK_MODE; see its comment on
	// d2cProvider). Zero value is false so existing construction sites
	// keep today's behavior; routes.go sets this explicitly from the same
	// d2cProvider == nil check that gates the bank-link routes below.
	//
	// Signup used to succeed unconditionally: a real user got an org,
	// an employee, a user row, and a live JWT, then hit a bare 404 on
	// every /worker/d2c/bank-link/* call afterward, because those routes
	// were never registered without a provider. Nothing ever told them
	// signup had handed them a dead end — found live during a
	// multi-persona review, not by a passing test. Checking this first
	// means a real user is told upfront, instead of being issued an
	// identity that can never link an account, get eligibility, or
	// receive an advance.
	BankLinkUnavailable bool
}

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
	if h.BankLinkUnavailable {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "d2c signup is not available yet — bank-account linking has no live provider configured"})
		return
	}

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
			ConsentType:    models.ConsentTypeD2CDisclosure,
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
		// A duplicate phone number is an ordinary, expected outcome — a
		// worker retrying a failed signup, or mistyping and trying again —
		// not a system fault. users.phone has a real UNIQUE constraint
		// (migration 000004) that this is the only guard against; without
		// checking for it specifically here, Postgres's constraint
		// violation fell through to the generic branch below as a 500,
		// which is what a real client actually saw. Caught live during a
		// multi-persona review, not by a passing test.
		var pgErr *pgconn.PgError
		if errors.As(txErr, &pgErr) && pgErr.Code == pgUniqueViolation {
			c.JSON(http.StatusConflict, gin.H{"error": "an account with this phone number already exists"})
			return
		}
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
