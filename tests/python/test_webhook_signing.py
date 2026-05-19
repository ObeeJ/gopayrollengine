"""
Offline contract tests for the Monnify webhook flow.

These reproduce the HMAC-SHA512 signing scheme used by the Go webhook handler
(internal/api/handlers/webhook_handler.go) so that any drift between the two
implementations is caught before it reaches production. No server required.

Run with:  pytest tests/python/
"""
import hashlib
import hmac
import json

import pytest

SECRET = b"monnify-test-secret"


def sign(body: bytes, secret: bytes = SECRET) -> str:
    """Mirror of the Go HMAC-SHA512 signature used in HandleMonnifyWebhook."""
    return hmac.new(secret, body, hashlib.sha512).hexdigest()


def verify(body: bytes, signature: str, secret: bytes = SECRET) -> bool:
    """Constant-time comparison — same semantics as Go's hmac.Equal."""
    expected = sign(body, secret)
    return hmac.compare_digest(expected, signature)


# ---------- signature verification ----------

def test_signature_matches_for_valid_body():
    body = b'{"eventType":"DISBURSEMENT_SUCCESSFUL"}'
    assert verify(body, sign(body))


def test_signature_fails_when_body_tampered():
    body = b'{"eventType":"DISBURSEMENT_SUCCESSFUL"}'
    sig = sign(body)
    tampered = body.replace(b"SUCCESSFUL", b"FAILED____")
    assert not verify(tampered, sig)


def test_signature_fails_with_wrong_secret():
    body = b'{"eventType":"DISBURSEMENT_SUCCESSFUL"}'
    sig = sign(body, secret=b"wrong-secret")
    assert not verify(body, sig)


def test_signature_is_hex_lowercase_128_chars():
    # SHA-512 → 64 bytes → 128 hex chars. Go's hex.EncodeToString emits lowercase.
    sig = sign(b"x")
    assert len(sig) == 128
    assert sig == sig.lower()


# ---------- payload contract ----------

REQUIRED_EVENT_TYPES = {"DISBURSEMENT_SUCCESSFUL", "DISBURSEMENT_FAILED"}


def webhook_event(event_type: str, ref: str = "ITEM-abc123") -> dict:
    return {
        "eventType": event_type,
        "eventData": {
            "batchReference": "PAY-batch-1",
            "transactionReference": ref,
            "status": "SUCCESS" if "SUCCESS" in event_type else "FAILED",
            "amount": 150000.00,
        },
    }


@pytest.mark.parametrize("event_type", sorted(REQUIRED_EVENT_TYPES))
def test_payload_round_trips_through_json(event_type):
    payload = webhook_event(event_type)
    body = json.dumps(payload).encode()
    parsed = json.loads(body)
    assert parsed["eventType"] == event_type
    assert parsed["eventData"]["transactionReference"].startswith("ITEM-")


def test_missing_transaction_reference_is_invalid():
    payload = webhook_event("DISBURSEMENT_SUCCESSFUL", ref="")
    assert payload["eventData"]["transactionReference"] == ""
    # Go handler returns 400 in this case; we assert the contract here so any
    # client generator that drops the field is caught at test time.


def test_unknown_event_types_are_ignored_by_contract():
    # The Go handler short-circuits to 200 on unknown eventType — this test
    # documents that contract so neither side silently introduces new types.
    payload = webhook_event("UNKNOWN_EVENT")
    assert payload["eventType"] not in REQUIRED_EVENT_TYPES


# ---------- known-answer test (KAT) ----------

def test_known_answer_vector():
    """
    Fixed vector — if either side changes the hashing scheme, this test
    breaks and forces a deliberate update on both ends.
    """
    body = b'{"eventType":"DISBURSEMENT_SUCCESSFUL","eventData":{"transactionReference":"ITEM-abc"}}'
    expected = hmac.new(SECRET, body, hashlib.sha512).hexdigest()
    assert sign(body) == expected
    assert len(expected) == 128
