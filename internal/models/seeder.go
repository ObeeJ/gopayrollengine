package models

import (
	"context"
	"log"
	"time"

	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// demoOrgID is the tenant the seeder writes into. Stable so re-runs are idempotent
// and so demo logins (which expect a specific org claim) line up with the data.
const demoOrgID = "ORG-DEMO-0001"

// SeedDB populates the database with mock employees and 3 months of completed
// payroll history. Every write runs inside models.WithOrgScope so the strict
// RLS policies from migration 000010 accept the inserts and so the rows are
// queryable through normal tenant-scoped reads afterwards.
func SeedDB() {
	ctx := context.Background()

	// 1. Demo organisation row. The organizations table is not yet covered by
	// RLS, so a direct insert is fine — and it must happen before WithOrgScope
	// because the policy's WITH CHECK references organization_id.
	if err := DB.Exec(
		`INSERT INTO organizations (id, name, created_at, updated_at)
		 VALUES (?, ?, NOW(), NOW())
		 ON CONFLICT (id) DO NOTHING`,
		demoOrgID, "Demo Organization",
	).Error; err != nil {
		log.Printf("Could not seed organization: %v", err)
		return
	}

	employees := []Employee{
		{OrganizationID: demoOrgID, Name: "John Doe", Email: "john@example.com", AccountNumber: "0123456789", BankCode: "058", Salary: money.FromNaira(250000), IsActive: true},
		{OrganizationID: demoOrgID, Name: "Jane Smith", Email: "jane@example.com", AccountNumber: "9876543210", BankCode: "011", Salary: money.FromNaira(350000), IsActive: true},
		{OrganizationID: demoOrgID, Name: "Bob Wilson", Email: "bob@example.com", AccountNumber: "1122334455", BankCode: "044", Salary: money.FromNaira(150000), IsActive: true},
	}

	if err := WithOrgScope(ctx, demoOrgID, func(tx *gorm.DB) error {
		for i := range employees {
			emp := &employees[i]
			// Lookup by the deterministic blind index — Email itself is now random-
			// nonce ciphertext and equal plaintexts yield different stored values.
			hmacDigest := BlindIndex(string(emp.Email))
			if err := tx.Where("email_hmac = ?", hmacDigest).FirstOrCreate(emp).Error; err != nil {
				log.Printf("Could not seed employee %s: %v", emp.Name, err)
			}
		}

		// 3 months of completed payroll history used by the analytics service.
		periods := []string{"February 2026", "March 2026", "April 2026"}
		for i, period := range periods {
			payroll := Payroll{
				OrganizationID: demoOrgID,
				Period:         period,
				TotalAmount:    money.FromNaira(750000),
				Status:         PayrollCompleted,
				CreatedAt:      time.Now().AddDate(0, -3+i, 0),
			}
			if err := tx.FirstOrCreate(&payroll, Payroll{OrganizationID: demoOrgID, Period: period}).Error; err != nil {
				log.Printf("Could not seed payroll %s: %v", period, err)
				continue
			}

			for _, emp := range employees {
				var dbEmp Employee
				if err := tx.Where("email_hmac = ?", BlindIndex(string(emp.Email))).First(&dbEmp).Error; err != nil {
					continue
				}
				item := PayrollItem{
					OrganizationID: demoOrgID,
					PayrollID:      payroll.ID,
					EmployeeID:     dbEmp.ID,
					EmployeeName:   dbEmp.Name,
					Amount:         dbEmp.Salary,
					Status:         PayrollCompleted,
				}
				tx.FirstOrCreate(&item, PayrollItem{PayrollID: payroll.ID, EmployeeID: dbEmp.ID})
			}
		}
		return nil
	}); err != nil {
		log.Printf("Seeder transaction failed: %v", err)
		return
	}

	log.Println("Database seeded with mock employees and 3 months of history.")
}
