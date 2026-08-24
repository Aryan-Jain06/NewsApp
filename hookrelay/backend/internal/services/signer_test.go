package services

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildBodyIsDeterministic(t *testing.T) {
	ts := time.Unix(1750000000, 0)
	data := json.RawMessage(`{"order_id":"ord_1","amount":499}`)

	first, err := BuildBody("evt_1", "order.created", ts, data)
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}
	second, err := BuildBody("evt_1", "order.created", ts, data)
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("body is not deterministic:\n%s\n%s", first, second)
	}

	want := `{"id":"evt_1","event_type":"order.created","timestamp":1750000000,"data":{"order_id":"ord_1","amount":499}}`
	if string(first) != want {
		t.Errorf("unexpected body\n got: %s\nwant: %s", first, want)
	}
}

func TestSigningPayloadFormat(t *testing.T) {
	got := string(SigningPayload("evt_1", 1750000000, []byte(`{"a":1}`)))
	want := `evt_1.1750000000.{"a":1}`
	if got != want {
		t.Errorf("signing payload\n got: %q\nwant: %q", got, want)
	}
}

func TestSignIsStableAndVersioned(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"id":"evt_1","event_type":"order.created","timestamp":1750000000,"data":{}}`)

	sig := Sign(secret, "evt_1", 1750000000, body)
	if !strings.HasPrefix(sig, "v1=") {
		t.Fatalf("signature %q is missing the v1= prefix", sig)
	}
	// 32-byte HMAC-SHA256 hex-encodes to 64 characters.
	if hex := strings.TrimPrefix(sig, "v1="); len(hex) != 64 {
		t.Errorf("expected 64 hex chars, got %d (%q)", len(hex), hex)
	}
	if again := Sign(secret, "evt_1", 1750000000, body); again != sig {
		t.Errorf("signature is not stable: %q != %q", again, sig)
	}
}

func TestSignatureIsSensitiveToEveryField(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"a":1}`)
	base := Sign(secret, "evt_1", 1750000000, body)

	cases := map[string]string{
		"different id":        Sign(secret, "evt_2", 1750000000, body),
		"different timestamp": Sign(secret, "evt_1", 1750000001, body),
		"different body":      Sign(secret, "evt_1", 1750000000, []byte(`{"a":2}`)),
		"different secret":    Sign("whsec_other", "evt_1", 1750000000, body),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s produced the same signature; the field is not covered by the HMAC", name)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"a":1}`)
	header := Sign(secret, "evt_1", 1750000000, body)

	if !VerifySignature(secret, "evt_1", header, 1750000000, body) {
		t.Error("valid signature was rejected")
	}
	if VerifySignature("whsec_wrong", "evt_1", header, 1750000000, body) {
		t.Error("signature verified under the wrong secret")
	}
	if VerifySignature(secret, "evt_1", header, 1750000000, []byte(`{"a":2}`)) {
		t.Error("tampered body still verified")
	}
	if VerifySignature(secret, "evt_1", "v1=deadbeef", 1750000000, body) {
		t.Error("garbage signature verified")
	}
	if VerifySignature(secret, "evt_1", "", 1750000000, body) {
		t.Error("empty signature header verified")
	}
}

func TestSignatureHeaderCarriesBothSecretsDuringRotation(t *testing.T) {
	const (
		newSecret = "whsec_new"
		oldSecret = "whsec_old"
	)
	body := []byte(`{"a":1}`)
	header := SignatureHeader([]string{newSecret, oldSecret}, "evt_1", 1750000000, body)

	if n := len(strings.Fields(header)); n != 2 {
		t.Fatalf("expected 2 space-separated signatures, got %d (%q)", n, header)
	}
	// A receiver holding either secret must be able to verify.
	if !VerifySignature(newSecret, "evt_1", header, 1750000000, body) {
		t.Error("new secret could not verify the rotation header")
	}
	if !VerifySignature(oldSecret, "evt_1", header, 1750000000, body) {
		t.Error("old secret could not verify the rotation header")
	}
	if VerifySignature("whsec_unrelated", "evt_1", header, 1750000000, body) {
		t.Error("an unrelated secret verified the rotation header")
	}
}

func TestSignatureHeaderSkipsEmptySecrets(t *testing.T) {
	header := SignatureHeader([]string{"whsec_only", ""}, "evt_1", 1750000000, []byte(`{}`))
	if n := len(strings.Fields(header)); n != 1 {
		t.Errorf("expected 1 signature, got %d (%q)", n, header)
	}
}

func TestVerifyRequestRejectsStaleTimestamps(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"a":1}`)
	now := time.Unix(1750000000, 0)

	fresh := now.Add(-30 * time.Second)
	sig := Sign(secret, "evt_1", fresh.Unix(), body)
	if err := VerifyRequest(secret, "evt_1", sig, strconv.FormatInt(fresh.Unix(), 10), body, now, DefaultToleranceWindow); err != nil {
		t.Errorf("fresh request rejected: %v", err)
	}

	// A captured request replayed an hour later must be refused even though its
	// signature is still cryptographically valid.
	stale := now.Add(-time.Hour)
	staleSig := Sign(secret, "evt_1", stale.Unix(), body)
	err := VerifyRequest(secret, "evt_1", staleSig, strconv.FormatInt(stale.Unix(), 10), body, now, DefaultToleranceWindow)
	if err == nil {
		t.Error("replayed request with a valid signature was accepted")
	}

	if err := VerifyRequest(secret, "evt_1", sig, "not-a-number", body, now, DefaultToleranceWindow); err == nil {
		t.Error("malformed timestamp header was accepted")
	}
}
