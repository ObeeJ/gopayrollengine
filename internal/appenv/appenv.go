// Package appenv decides whether the process may fall back to insecure
// development defaults (zero encryption keys, a hardcoded JWT secret,
// MOCK_MODE money movement).
//
// Every guard used to test APP_ENV == "production", which made "unset" or a
// typo like "prod" behave as development: a deployment that forgot APP_ENV
// silently ran with a zero AES key and a public JWT secret. The rule is now
// inverted — insecure defaults are opt-in, and only for environments that
// name themselves as non-production.
package appenv

import (
	"fmt"
	"os"
	"testing"
)

const (
	Development = "development"
	Test        = "test"
	Staging     = "staging"
	Production  = "production"
)

// Name returns APP_ENV as set.
func Name() string { return os.Getenv("APP_ENV") }

// AllowsInsecureDefaults reports whether dev fallbacks are permitted. Only an
// explicit development/test APP_ENV qualifies — plus an unset APP_ENV inside
// a `go test` binary, so unit tests don't each need to set it. Staging,
// production, unset, and anything unrecognised all fail closed.
func AllowsInsecureDefaults() bool {
	switch Name() {
	case Development, Test:
		return true
	case "":
		return testing.Testing()
	default:
		return false
	}
}

// Validate refuses an unset or unrecognised APP_ENV. Called once at process
// start so a misconfigured deployment dies loudly instead of guessing.
func Validate() error {
	switch Name() {
	case Development, Test, Staging, Production:
		return nil
	case "":
		return fmt.Errorf("APP_ENV is not set; must be one of %s, %s, %s, %s",
			Development, Test, Staging, Production)
	default:
		return fmt.Errorf("APP_ENV=%q is not recognised; must be one of %s, %s, %s, %s",
			Name(), Development, Test, Staging, Production)
	}
}
