package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureSender struct {
	codes chan string
}

func (s *captureSender) SendOTP(_ context.Context, _, code string) error {
	s.codes <- code
	return nil
}

func newTestOTP(t *testing.T) (*OTPService, *captureSender, *miniredis.Miniredis) {
	t.Helper()
	models.InitEncryption()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	sender := &captureSender{codes: make(chan string, 16)}
	return NewOTPService(rdb, sender), sender, mr
}

func receive(t *testing.T, s *captureSender) string {
	t.Helper()
	select {
	case c := <-s.codes:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no otp was sent")
		return ""
	}
}

func TestOTP_CorrectCodeVerifiesOnce(t *testing.T) {
	svc, sender, _ := newTestOTP(t)
	ctx := context.Background()

	require.NoError(t, svc.Issue(ctx, "+2348010000001", true))
	code := receive(t, sender)
	assert.Len(t, code, 6)

	ok, err := svc.Verify(ctx, "+2348010000001", code)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = svc.Verify(ctx, "+2348010000001", code)
	require.NoError(t, err)
	assert.False(t, ok, "a code must be single-use")
}

func TestOTP_CodeIsBoundToItsPhone(t *testing.T) {
	svc, sender, _ := newTestOTP(t)
	ctx := context.Background()

	require.NoError(t, svc.Issue(ctx, "+2348010000002", true))
	code := receive(t, sender)

	ok, err := svc.Verify(ctx, "+2348010000003", code)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestOTP_WrongGuessesBurnTheCode(t *testing.T) {
	svc, sender, _ := newTestOTP(t)
	ctx := context.Background()

	require.NoError(t, svc.Issue(ctx, "+2348010000004", true))
	code := receive(t, sender)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < maxOTPAttempts; i++ {
		ok, err := svc.Verify(ctx, "+2348010000004", wrong)
		require.NoError(t, err)
		assert.False(t, ok)
	}
	ok, err := svc.Verify(ctx, "+2348010000004", code)
	require.NoError(t, err)
	assert.False(t, ok, "the right code must not work after the attempt cap is spent")
}

func TestOTP_ExpiresAfterTTL(t *testing.T) {
	svc, sender, mr := newTestOTP(t)
	ctx := context.Background()

	require.NoError(t, svc.Issue(ctx, "+2348010000005", true))
	code := receive(t, sender)
	mr.FastForward(otpTTL + time.Second)

	ok, err := svc.Verify(ctx, "+2348010000005", code)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestOTP_ResendCooldownAndDailyCap(t *testing.T) {
	svc, sender, mr := newTestOTP(t)
	ctx := context.Background()
	phone := "+2348010000006"

	require.NoError(t, svc.Issue(ctx, phone, true))
	receive(t, sender)
	assert.ErrorIs(t, svc.Issue(ctx, phone, true), ErrOTPRateLimited)

	for i := 1; i < maxOTPIssuesPerDay; i++ {
		mr.FastForward(otpResendCooldown + time.Second)
		require.NoError(t, svc.Issue(ctx, phone, true))
		receive(t, sender)
	}
	mr.FastForward(otpResendCooldown + time.Second)
	assert.ErrorIs(t, svc.Issue(ctx, phone, true), ErrOTPRateLimited)
}

func TestOTP_UnknownPhoneIsThrottledButNothingIsSent(t *testing.T) {
	svc, sender, _ := newTestOTP(t)
	ctx := context.Background()

	require.NoError(t, svc.Issue(ctx, "+2348010000007", false))
	assert.ErrorIs(t, svc.Issue(ctx, "+2348010000007", false), ErrOTPRateLimited,
		"unknown phones must be throttled exactly like known ones")
	select {
	case <-sender.codes:
		t.Fatal("no code may be sent for an unknown phone")
	case <-time.After(100 * time.Millisecond):
	}
	ok, err := svc.Verify(ctx, "+2348010000007", "123456")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestOTP_NoSenderFailsClosed(t *testing.T) {
	models.InitEncryption()
	mr := miniredis.RunT(t)
	svc := NewOTPService(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil)

	assert.ErrorIs(t, svc.Issue(context.Background(), "+2348010000008", true), ErrOTPUnavailable)
	ok, err := svc.Verify(context.Background(), "+2348010000008", "123456")
	assert.ErrorIs(t, err, ErrOTPUnavailable)
	assert.False(t, ok)
}
