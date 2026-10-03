package handlers

import (
	"fmt"
	"math/rand"
)

// testPhone — a fresh, valid E.164 Nigerian mobile number per call, so
// integration tests don't collide on users.phone's UNIQUE constraint.
func testPhone() string {
	return fmt.Sprintf("+23480%08d", rand.Intn(100_000_000))
}
