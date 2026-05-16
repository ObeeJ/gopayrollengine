package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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

// InitEncryption — loads the KEK or dies; plaintext PII in prod is not an option.
func InitEncryption() {
	hexKey := os.Getenv("ENCRYPTION_KEK")
	if hexKey == "" {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("FATAL: ENCRYPTION_KEK is not set. Refusing to start in production.")
		}
		// Dev fallback — 32 zero bytes. Loud, obvious, never mistaken for real security.
		log.Println("WARNING: ENCRYPTION_KEK not set — using insecure dev key. Set it before going live.")
		kek = make([]byte, 32)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(hexKey)
	if err != nil || len(decoded) != 32 {
		log.Fatal("FATAL: ENCRYPTION_KEK must be a base64-encoded 32-byte key.")
	}
	kek = decoded
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

// MarshalJSON — serialises as a plain string so API responses look normal.
func (e EncryptedString) MarshalJSON() ([]byte, error) {
	return []byte(`"` + string(e) + `"`), nil
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
