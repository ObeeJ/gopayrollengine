package monnify

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateReservedAccount_MockModeReturnsAnAccount(t *testing.T) {
	c := &Client{MockMode: true}
	resp, err := c.CreateReservedAccount(ReserveAccountRequest{
		AccountReference: "FUND-ORG-TEST",
		AccountName:      "Test Org",
		CurrencyCode:     "NGN",
	})
	require.NoError(t, err)
	assert.True(t, resp.RequestSuccessful)
	assert.Equal(t, "FUND-ORG-TEST", resp.ResponseBody.AccountReference)
	require.Len(t, resp.ResponseBody.Accounts, 1)
	assert.NotEmpty(t, resp.ResponseBody.Accounts[0].AccountNumber)
}
