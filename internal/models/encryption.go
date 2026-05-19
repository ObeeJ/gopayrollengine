package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
)

// kek is the Key Encryption Key loaded once at startup from ENCRYPTION_KEK.
// In production, swap this for an AWS KMS data key — the interface stays the same.
var kek []byte

// hmacKey is a separate key used for deterministic blind-indexing of PII
// fields that must remain queryable (e.g. email lookups, uniqueness). Using
// the same key as the KEK would compromise both: HMAC outputs are stored
// alongside ciphertext and an attacker with database read access could mount
// a known-plaintext attack against the encryption key. Independent keys
// preserve confidentiality even if one is exposed.
var hmacKey []byte

// InitEncryption — loads the KEK and the blind-index HMAC key, or dies;
// plaintext PII in prod is not an option.
func InitEncryption() {
	hexKey := os.Getenv("ENCRYPTION_KEK")
	if hexKey == "" {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("FATAL: ENCRYPTION_KEK is not set. Refusing to start in production.")
		}
		// Dev fallback — 32 zero bytes. Loud, obvious, never mistaken for real security.
		log.Println("WARNING: ENCRYPTION_KEK not set — using insecure dev key. Set it before going live.")
		kek = make([]byte, 32)
	} else {
		decoded, err := base64.StdEncoding.DecodeString(hexKey)
		if err != nil || len(decoded) != 32 {
			log.Fatal("FATAL: ENCRYPTION_KEK must be a base64-encoded 32-byte key.")
		}
		kek = decoded
	}

	hexHMAC := os.Getenv("ENCRYPTION_HMAC_KEY")
	if hexHMAC == "" {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("FATAL: ENCRYPTION_HMAC_KEY is not set. Required for PII blind indexing.")
		}
		log.Println("WARNING: ENCRYPTION_HMAC_KEY not set — using insecure dev HMAC key.")
		hmacKey = make([]byte, 32)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(hexHMAC)
	if err != nil || len(decoded) != 32 {
		log.Fatal("FATAL: ENCRYPTION_HMAC_KEY must be a base64-encoded 32-byte key.")
	}
	hmacKey = decoded
}

// BlindIndex returns the HMAC-SHA256 digest of plaintext under the dedicated
// HMAC key. Deterministic — equal plaintexts yield equal digests — so a column
// of these supports equality lookups and unique constraints over otherwise
// non-deterministic ciphertext. Used for the (org_id, email_hmac) unique index
// and any future PII field that must remain searchable.
func BlindIndex(plaintext string) []byte {
	if plaintext == "" {
		return nil
	}
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(plaintext))
	return mac.Sum(nil)
}

// encrypt — AES-256-GCM: authenticated encryption so tampering is detectable, not just unreadable.
func encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	// Nonce is prepended to ciphertext so Decrypt can extract it without a separate column.
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt — reverses encrypt; returns an error if the ciphertext was tampered with.
func decrypt(encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// EncryptedString — a string that encrypts itself on the way into the DB and decrypts on the way out.
// Drop it in any model field that holds PII; the rest of the code stays unchanged.
type EncryptedString string

// Value — called by GORM/database/sql before writing; encrypts the plaintext value.
func (e EncryptedString) Value() (driver.Value, error) {
	if e == "" {
		return "", nil
	}
	return encrypt(string(e))
}

// Scan — called by GORM/database/sql after reading; decrypts the stored ciphertext.
func (e *EncryptedString) Scan(value interface{}) error {
	if value == nil {
		*e = ""
		return nil
	}
	str, ok := value.(string)
	if !ok {
		return fmt.Errorf("EncryptedString: expected string from DB, got %T", value)
	}
	if str == "" {
		*e = ""
		return nil
	}
	plain, err := decrypt(str)
	if err != nil {
		return fmt.Errorf("EncryptedString: decryption failed: %w", err)
	}
	*e = EncryptedString(plain)
	return nil
}

// MarshalJSON returns a masked representation of the value — never the raw
// plaintext. PII fields (account number, bank code, BVN, etc.) are
// decrypted in-process so handlers can use them, but they MUST NOT travel
// back out of the API in clear form. We expose at most the last four
// characters so an authenticated user can still recognise their own record;
// shorter values are fully masked.
//
// If a caller needs the plaintext (e.g. building a Monnify payload), call
// .String() explicitly — that keeps the leak surface visible at the call site
// rather than implicit in every JSON response.
func (e EncryptedString) MarshalJSON() ([]byte, error) {
	return []byte(`"` + maskPII(string(e)) + `"`), nil
}

// maskPII keeps the last four characters visible, replaces the rest with
// asterisks, and fully masks values that are too short for the suffix to
// retain any identifying value.
func maskPII(s string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// UnmarshalJSON — deserialises from a plain string; encryption happens at DB write time.
func (e *EncryptedString) UnmarshalJSON(data []byte) error {
	if len(data) >= 2 && data[0] == '"' {
		*e = EncryptedString(data[1 : len(data)-1])
	}
	return nil
}

// String — returns the plaintext value; safe for display, never for logging.
func (e EncryptedString) String() string { return string(e) }
