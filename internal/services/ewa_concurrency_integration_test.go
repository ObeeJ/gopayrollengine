//go:build integration

package services

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Several simultaneous requests, each for the worker's entire available
// balance, must approve at most one: eligibility is read-then-insert, so
// without serialization every request reads the same "nothing outstanding
// yet" state and each one approves the full amount — a worker could draw a
// multiple of what they have actually earned by firing requests in parallel.
func TestRequestAdvance_ConcurrentRequestsCannotOverdraw(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	if el.Blocked || el.Available < el.MinimumDraw {
		t.Skipf("nothing drawable at this point in the period (available=%s)", el.Available)
	}

	const parallel = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, _ = svc.RequestAdvance(context.Background(), orgID, employeeID,
				el.Available, fmt.Sprintf("race-%d", i), "127.0.0.1")
		}(i)
	}
	close(start)
	wg.Wait()

	var approved []models.EWAAdvance
	require.NoError(t, models.DB.
		Where("employee_id = ? AND status = ?", employeeID, models.AdvanceApproved).
		Find(&approved).Error)

	amounts := make([]money.Kobo, 0, len(approved))
	for _, a := range approved {
		amounts = append(amounts, a.AmountKobo)
	}
	total, err := money.Sum(amounts)
	require.NoError(t, err)

	assert.Len(t, approved, 1, "exactly one full-balance request may be approved")
	assert.LessOrEqual(t, int64(total), int64(el.Available),
		"approved total (%s) must never exceed what was available (%s)", total, el.Available)
}
