package models

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Use a deterministic 32-byte key for tests so InitEncryption is idempotent.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	os.Setenv("ENCRYPTION_KEK", base64.StdEncoding.EncodeToString(key))
	InitEncryption()
}

func TestEncryptedString_RoundTrip(t *testing.T) {
	plain := "0123456789"
	v, err := EncryptedString(plain).Value()
	require.NoError(t, err)

	ciphertext, ok := v.(string)
	require.True(t, ok)
	assert.NotEqual(t, plain, ciphertext, "Value() must produce ciphertext, not plaintext")

	var decoded EncryptedString
	require.NoError(t, decoded.Scan(ciphertext))
	assert.Equal(t, plain, string(decoded))
}

func TestEncryptedString_EmptyValue(t *testing.T) {
	v, err := EncryptedString("").Value()
	require.NoError(t, err)
	assert.Equal(t, "", v)

	var dst EncryptedString
	require.NoError(t, dst.Scan(""))
	assert.Equal(t, "", string(dst))

	require.NoError(t, dst.Scan(nil))
	assert.Equal(t, "", string(dst))
}

func TestEncryptedString_ScanRejectsNonString(t *testing.T) {
	var dst EncryptedString
	err := dst.Scan(42)
	assert.Error(t, err)
}

func TestEncryptedString_TamperedCiphertextFails(t *testing.T) {
	v, err := EncryptedString("secret").Value()
	require.NoError(t, err)
	ct := v.(string)

	// Flip a byte inside the ciphertext — GCM tag must catch it.
	raw, err := base64.StdEncoding.DecodeString(ct)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.StdEncoding.EncodeToString(raw)

	var dst EncryptedString
	err = dst.Scan(tampered)
	assert.Error(t, err)
}

// TestEncryptedString_JSONMasksPlaintext locks in the Wave 2 #3 fix: the
// previous MarshalJSON returned raw plaintext, leaking PII into every API
// response that embedded an account number or bank code. The fixed
// implementation must emit a masked form, preserving only the trailing four
// characters so an authenticated owner can still recognise their record.
func TestEncryptedString_JSONMasksPlaintext(t *testing.T) {
	type wrap struct {
		Acc EncryptedString `json:"acc"`
	}
	w := wrap{Acc: "1234567890"}
	b, err := json.Marshal(w)
	require.NoError(t, err)
	assert.JSONEq(t, `{"acc":"****7890"}`, string(b))

	// Short values are fully masked — a 4-char suffix on a 4-char value
	// would reveal the whole thing.
	short := wrap{Acc: "1234"}
	b, err = json.Marshal(short)
	require.NoError(t, err)
	assert.JSONEq(t, `{"acc":"****"}`, string(b))

	// Empty stays empty.
	empty := wrap{Acc: ""}
	b, err = json.Marshal(empty)
	require.NoError(t, err)
	assert.JSONEq(t, `{"acc":""}`, string(b))

	// JSON ingress is unchanged — masking is one-way for output only.
	var back wrap
	require.NoError(t, json.Unmarshal([]byte(`{"acc":"1234567890"}`), &back))
	assert.Equal(t, EncryptedString("1234567890"), back.Acc)
}

// TestBlindIndex_Deterministic locks in the property that makes the
// (org_id, email_hmac) unique index work: equal plaintexts must produce
// equal digests, different plaintexts must not, and empty input must not
// collide with anything (returns nil so partial-index WHERE excludes it).
func TestBlindIndex_Deterministic(t *testing.T) {
	a := BlindIndex("alice@example.com")
	b := BlindIndex("alice@example.com")
	c := BlindIndex("bob@example.com")
	assert.Equal(t, a, b, "equal plaintexts must produce equal digests")
	assert.NotEqual(t, a, c, "different plaintexts must produce different digests")
	assert.Nil(t, BlindIndex(""), "empty plaintext must return nil so the partial unique index excludes it")
}

func TestEncryptedString_NonceVariesPerCall(t *testing.T) {
	a, err := EncryptedString("same").Value()
	require.NoError(t, err)
	b, err := EncryptedString("same").Value()
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "GCM nonce must be random — same plaintext must not produce same ciphertext")
}
