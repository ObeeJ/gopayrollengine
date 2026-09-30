package services

import (
	"errors"
	"testing"
)

// The (organization_id, period) unique index is the only thing stopping a
// month being paid twice, and it compares strings — so only the one canonical
// spelling may ever be accepted.
func TestValidatePeriod(t *testing.T) {
	for _, ok := range []string{"2026-09", "2026-12", "1999-01"} {
		if err := ValidatePeriod(ok); err != nil {
			t.Errorf("ValidatePeriod(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"2026-9", "Sep 2026", "2026-13", "2026-00", "2026-09-01", " 2026-09", "2026/09", "", "banana"} {
		if err := ValidatePeriod(bad); !errors.Is(err, ErrInvalidPeriod) {
			t.Errorf("ValidatePeriod(%q) = %v, want ErrInvalidPeriod", bad, err)
		}
	}
}
