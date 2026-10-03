package handlers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Guard rail: a handler may return a *sentinel* error's message to the client
// (those are written for people), but must never pass the raw text of an
// unknown error — SQL, hostnames, provider payloads — through to the response.
// The two sites that did (analytics 500, funding 502) are the reason this
// exists. This is a source scan: cheap, and it fails at the exact line.
func TestHandlersNeverEchoRawErrorsOnServerFailures(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	// 5xx responses must never contain err.Error().
	serverErr := regexp.MustCompile(`c\.JSON\(http\.Status(InternalServerError|BadGateway|ServiceUnavailable|GatewayTimeout)[^\n]*err\.Error\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		for n, line := range strings.Split(string(raw), "\n") {
			assert.False(t, serverErr.MatchString(line), "%s:%d returns a raw error on a 5xx: %s", f, n+1, strings.TrimSpace(line))
		}
	}
}

// And a binding failure must go through respondBindError, never err.Error().
func TestHandlersNeverEchoRawBindingErrors(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	bind := regexp.MustCompile(`ShouldBind\w*\([^)]*\); err != nil \{\n\s*c\.JSON\([^\n]*err\.Error\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.False(t, bind.Match(raw), "%s writes a raw ShouldBind error to the client; use respondBindError", f)
	}
}
