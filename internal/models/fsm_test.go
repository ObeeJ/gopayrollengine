package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanTransition_ValidMoves(t *testing.T) {
	valid := []struct{ from, to PayrollStatus }{
		{PayrollPending, PayrollProcessing},
		{PayrollPending, PayrollFailed},
		{PayrollProcessing, PayrollCompleted},
		{PayrollProcessing, PayrollFailed},
		{PayrollFailed, PayrollPending},
		{PayrollFailed, PayrollProcessing}, // Asynq retry edge — Fix #5
	}
	for _, tc := range valid {
		assert.True(t, CanTransition(tc.from, tc.to), "%s -> %s should be allowed", tc.from, tc.to)
	}
}

func TestCanTransition_InvalidMoves(t *testing.T) {
	invalid := []struct{ from, to PayrollStatus }{
		{PayrollCompleted, PayrollPending},
		{PayrollCompleted, PayrollProcessing},
		{PayrollCompleted, PayrollFailed},
		{PayrollPending, PayrollCompleted},
		{PayrollProcessing, PayrollPending},
		{PayrollFailed, PayrollCompleted}, // can't skip the worker
	}
	for _, tc := range invalid {
		assert.False(t, CanTransition(tc.from, tc.to), "%s -> %s should be forbidden", tc.from, tc.to)
	}
}

func TestCanTransition_SelfLoop(t *testing.T) {
	for _, s := range []PayrollStatus{PayrollPending, PayrollProcessing, PayrollCompleted, PayrollFailed} {
		assert.False(t, CanTransition(s, s), "self-transition on %s must not be allowed", s)
	}
}
