package services

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Signature header names sent with every delivery.
const (
	HeaderID        = "X-HookRelay-Id"
	HeaderTimestamp = "X-HookRelay-Timestamp"
	HeaderSignature = "X-HookRelay-Signature"
	HeaderEventType = "X-HookRelay-Event-Type"
	HeaderAttempt   = "X-HookRelay-Attempt"

	// SignatureVersion prefixes each signature so the scheme can evolve without
	// breaking existing receivers.
	SignatureVersion = "v1"
)

// DefaultToleranceWindow is how much clock skew a receiver should accept when
// checking HeaderTimestamp. Rejecting old timestamps is what stops a captured
// request from being replayed later.
const DefaultToleranceWindow = 5 * time.Minute

// WebhookBody is the JSON document POSTed to a subscriber.
type WebhookBody struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// BuildBody renders the canonical webhook body. Field order is fixed by the
// struct, so the same event always serialises to the same bytes — which matters
// because the signature covers those exact bytes.
func BuildBody(eventID, eventType string, ts time.Time, data json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(WebhookBody{
		ID:        eventID,
		EventType: eventType,
		Timestamp: ts.Unix(),
		Data:      data,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal webhook body: %w", err)
	}
	return body, nil
}

// SigningPayload is the exact string the HMAC is computed over:
//
//	"{id}.{timestamp}.{body}"
//
// Binding the id and timestamp into the signed material — rather than signing
// the body alone — is what makes each request unique. A body-only signature
// would stay valid forever and could be replayed verbatim; including the
// timestamp lets a receiver reject stale requests, and including the id lets it
// deduplicate them.
func SigningPayload(id string, ts int64, body []byte) []byte {
	var b strings.Builder
	b.Grow(len(id) + 24 + len(body))
	b.WriteString(id)
	b.WriteByte('.')
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteByte('.')
	b.Write(body)
	return []byte(b.String())
}

// Sign returns the "v1=<hex>" signature of body for one secret.
func Sign(secret, id string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SigningPayload(id, ts, body))
	return SignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader joins one signature per secret with spaces, newest first. A
// rotating endpoint therefore sends two, and a receiver that still holds either
// secret can verify the request.
func SignatureHeader(secrets []string, id string, ts int64, body []byte) string {
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		sigs = append(sigs, Sign(s, id, ts, body))
	}
	return strings.Join(sigs, " ")
}

// VerifySignature reports whether header carries a signature matching secret.
// The comparison is constant time so a receiver cannot be used as an oracle.
func VerifySignature(secret, id, header string, ts int64, body []byte) bool {
	want := Sign(secret, id, ts, body)
	for _, got := range strings.Fields(header) {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

// VerifyRequest checks the signature and the timestamp freshness together. It is
// the reference implementation receivers should mirror.
func VerifyRequest(secret, id, sigHeader, tsHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	ts, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("timestamp header %q is not a unix timestamp: %w", tsHeader, err)
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return fmt.Errorf("timestamp is %s outside the %s tolerance window", skew.Round(time.Second), tolerance)
	}
	if !VerifySignature(secret, id, sigHeader, ts, body) {
		return fmt.Errorf("no signature in %q matches the expected value", HeaderSignature)
	}
	return nil
}
