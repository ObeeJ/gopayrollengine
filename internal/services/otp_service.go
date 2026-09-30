package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"go-payroll-engine/internal/models"

	"github.com/redis/go-redis/v9"
)

// OTP policy. A 6-digit code has 10^6 values; with maxOTPAttempts guesses per
// code and maxOTPIssuesPerDay codes per phone, an attacker gets at most 50
// guesses a day per phone — a ~0.005% daily chance, not a brute-force target.
const (
	otpDigits          = 6
	otpTTL             = 5 * time.Minute
	otpResendCooldown  = 60 * time.Second
	maxOTPAttempts     = 5
	maxOTPIssuesPerDay = 10
	otpSendTimeout     = 10 * time.Second
)

var (
	// ErrOTPUnavailable — no SMS provider is configured, so no code can be delivered.
	ErrOTPUnavailable = errors.New("otp delivery is not configured")
	// ErrOTPRateLimited — the phone is inside its resend cooldown or daily cap.
	ErrOTPRateLimited = errors.New("too many otp requests for this phone")
)

// OTPSender delivers a one-time code to a phone. A real SMS provider
// implements this; none is wired yet (see routes.go).
type OTPSender interface {
	SendOTP(ctx context.Context, phone, code string) error
}

// LogOTPSender writes the code to the process log. Development/test only —
// routes.go wires it solely under MOCK_MODE, which cmd/api refuses outside
// APP_ENV=development/test.
type LogOTPSender struct{}

func (LogOTPSender) SendOTP(_ context.Context, phone, code string) error {
	log.Printf("MOCK OTP for %s: %s", phone, code)
	return nil
}

// OTPService issues and verifies worker login codes. Codes live only in
// Redis, only as an HMAC (never plaintext), keyed by an HMAC of the phone,
// are single-use, expire after otpTTL, and die after maxOTPAttempts wrong
// guesses.
type OTPService struct {
	rdb    *redis.Client
	sender OTPSender
}

// NewOTPService — sender may be nil, in which case Issue returns ErrOTPUnavailable.
func NewOTPService(rdb *redis.Client, sender OTPSender) *OTPService {
	return &OTPService{rdb: rdb, sender: sender}
}

// Available reports whether codes can be delivered at all.
func (s *OTPService) Available() bool { return s != nil && s.rdb != nil && s.sender != nil }

func phoneKey(phone string) string {
	return hex.EncodeToString(models.BlindIndex("otp-phone:" + phone))
}

func codeDigest(phone, code string) string {
	return hex.EncodeToString(models.BlindIndex("otp-code:" + phone + ":" + code))
}

// Issue rate-limits, then — only when deliver is true — generates, stores and
// sends a code. The caller passes deliver=false for an unknown or inactive
// phone: the rate limit still applies and the call still succeeds, so the
// response doesn't reveal which phones have accounts.
func (s *OTPService) Issue(ctx context.Context, phone string, deliver bool) error {
	if !s.Available() {
		return ErrOTPUnavailable
	}
	k := phoneKey(phone)

	ok, err := s.rdb.SetNX(ctx, "otp:cooldown:"+k, 1, otpResendCooldown).Result()
	if err != nil {
		return fmt.Errorf("otp cooldown: %w", err)
	}
	if !ok {
		return ErrOTPRateLimited
	}
	n, err := s.rdb.Incr(ctx, "otp:daily:"+k).Result()
	if err != nil {
		return fmt.Errorf("otp daily cap: %w", err)
	}
	if n == 1 {
		s.rdb.Expire(ctx, "otp:daily:"+k, 24*time.Hour)
	}
	if n > maxOTPIssuesPerDay {
		return ErrOTPRateLimited
	}

	if !deliver {
		return nil
	}

	code, err := generateOTP()
	if err != nil {
		return err
	}
	// A new code replaces the old one and resets its attempt counter.
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, "otp:code:"+k, codeDigest(phone, code), otpTTL)
	pipe.Del(ctx, "otp:attempts:"+k)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("otp store: %w", err)
	}

	// Deliver off the request path: an SMS round-trip only for real accounts
	// would otherwise be a timing oracle for which phones are registered.
	// WithoutCancel: the send must outlive the request that triggered it.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), otpSendTimeout)
	go func() {
		defer cancel()
		if err := s.sender.SendOTP(sendCtx, phone, code); err != nil {
			log.Printf("otp send failed for phone key %s: %v", k, err)
		}
	}()
	return nil
}

// verifyScript — check-and-consume in one atomic step so two concurrent
// guesses can't both read the code before either deletes it, and the
// attempt counter can't be raced past its cap.
var verifyScript = redis.NewScript(`
local stored = redis.call('GET', KEYS[1])
if not stored then return 0 end
local n = redis.call('INCR', KEYS[2])
if n == 1 then redis.call('PEXPIRE', KEYS[2], ARGV[3]) end
if n > tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1], KEYS[2])
  return 0
end
if stored == ARGV[1] then
  redis.call('DEL', KEYS[1], KEYS[2])
  return 1
end
return 0
`)

// Verify reports whether code is the live code for phone, consuming it on
// success. Digests are compared, not codes, so comparison timing reveals
// nothing about the code itself.
func (s *OTPService) Verify(ctx context.Context, phone, code string) (bool, error) {
	if !s.Available() {
		return false, ErrOTPUnavailable
	}
	if len(code) != otpDigits {
		return false, nil
	}
	k := phoneKey(phone)
	res, err := verifyScript.Run(ctx, s.rdb,
		[]string{"otp:code:" + k, "otp:attempts:" + k},
		codeDigest(phone, code), maxOTPAttempts, otpTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("otp verify: %w", err)
	}
	return res == 1, nil
}

func generateOTP() (string, error) {
	max := big.NewInt(1_000_000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("otp generate: %w", err)
	}
	return fmt.Sprintf("%0*d", otpDigits, n.Int64()), nil
}
