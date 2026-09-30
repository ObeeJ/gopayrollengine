package workers

import (
	"encoding/json"
	"testing"

	"go-payroll-engine/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBVNTaskPayload_NeverCarriesPlaintextBVN(t *testing.T) {
	models.InitEncryption()
	raw, err := bvnTaskPayload("ORG-1", "EMP-1", "22233344455")
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "22233344455")

	var p map[string]string
	require.NoError(t, json.Unmarshal(raw, &p))
	plain, err := models.DecryptString(p["bvn_enc"])
	require.NoError(t, err)
	assert.Equal(t, "22233344455", plain)
}
