package models

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCanTransitionTimeEntry_ValidMoves(t *testing.T) {
	valid := []struct{ from, to TimeEntryStatus }{
		{TimeEntryPending, TimeEntryApproved},
		{TimeEntryPending, TimeEntryRejected},
	}
	for _, tc := range valid {
		assert.True(t, CanTransitionTimeEntry(tc.from, tc.to), "%s -> %s should be allowed", tc.from, tc.to)
	}
}

func TestCanTransitionTimeEntry_InvalidMoves(t *testing.T) {
	invalid := []struct{ from, to TimeEntryStatus }{
		{TimeEntryApproved, TimeEntryRejected},
		{TimeEntryApproved, TimeEntryPending},
		{TimeEntryRejected, TimeEntryApproved},
		{TimeEntryRejected, TimeEntryPending},
	}
	for _, tc := range invalid {
		assert.False(t, CanTransitionTimeEntry(tc.from, tc.to), "%s -> %s should be forbidden", tc.from, tc.to)
	}
}

func TestCanTransitionTimeEntry_SelfLoop(t *testing.T) {
	for _, s := range []TimeEntryStatus{TimeEntryPending, TimeEntryApproved, TimeEntryRejected} {
		assert.False(t, CanTransitionTimeEntry(s, s), "self-transition on %s must not be allowed", s)
	}
}

func TestSumApprovedMinutes_InvalidPeriod(t *testing.T) {
	_, err := SumApprovedMinutes(nil, "ORG-1", "EMP-1", "not-a-period", time.Now())
	assert.Error(t, err)
}
