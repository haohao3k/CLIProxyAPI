package cursor

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestPKCEChallengeS256(t *testing.T) {
	// Expected value computed independently with Python hashlib + urlsafe base64.
	const verifier = "dBjftJeZ4CVP-mJ92K9mK8_kc2vQkyIqpXqfAZoSD2zM"
	const want = "GaCqPCYkNePwWoBzcuw9rLq5_eUA_HQr-ziOGbg_Mow"
	if got := PKCEChallenge(verifier); got != want {
		t.Fatalf("PKCEChallenge() = %q, want %q", got, want)
	}
}

func TestBuildLoginURL(t *testing.T) {
	raw := BuildLoginURL("abc-_123", "11111111-2222-3333-4444-555555555555")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != loginPageURL {
		t.Fatalf("base = %q, want %q", got, loginPageURL)
	}
	q := u.Query()
	if q.Get("challenge") != "abc-_123" || q.Get("uuid") != "11111111-2222-3333-4444-555555555555" || q.Get("mode") != "login" {
		t.Fatalf("unexpected query: %v", q)
	}
}

func TestStartLoginFlowConsistency(t *testing.T) {
	flow, err := NewCursorAuth(nil).StartLoginFlow(context.Background())
	if err != nil {
		t.Fatalf("StartLoginFlow() error = %v", err)
	}
	if flow.Verifier == "" || strings.ContainsAny(flow.Verifier, "+/=") {
		t.Fatalf("verifier must be non-empty raw base64url, got %q", flow.Verifier)
	}
	if flow.Challenge != PKCEChallenge(flow.Verifier) {
		t.Fatalf("challenge does not match verifier")
	}
	if flow.LoginURL != BuildLoginURL(flow.Challenge, flow.UUID) {
		t.Fatalf("login url mismatch: %q", flow.LoginURL)
	}
	if flow.ExpiresIn <= 0 {
		t.Fatalf("expires_in must be positive")
	}
}

func TestParsePollResponse(t *testing.T) {
	result, err := ParsePollResponse([]byte(`{"accessToken":"tok123","authId":"auth0|user_42"}`))
	if err != nil {
		t.Fatalf("ParsePollResponse() error = %v", err)
	}
	if result == nil || result.AccessToken != "tok123" || result.AuthID != "auth0|user_42" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Cookie != "user_42%3A%3Atok123" {
		t.Fatalf("cookie = %q", result.Cookie)
	}

	pending, err := ParsePollResponse([]byte(`{"accessToken":""}`))
	if err != nil || pending != nil {
		t.Fatalf("pending poll: result=%+v err=%v", pending, err)
	}

	if _, err = ParsePollResponse([]byte(`not-json`)); err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestBuildMetadataAndLabel(t *testing.T) {
	bundle := &CursorAuthBundle{Login: newLoginResult("tok", "")}
	meta := BuildMetadata(bundle)
	if meta["type"] != "cursor" || meta["access_token"] != "tok" || meta["cookie"] != "tok" {
		t.Fatalf("unexpected metadata: %v", meta)
	}
	if _, ok := meta["auth_id"]; ok {
		t.Fatalf("empty auth_id must be omitted")
	}
	if bundle.Label() != "Cursor User" {
		t.Fatalf("label = %q", bundle.Label())
	}
	if got := (&CursorAuthBundle{Login: newLoginResult("tok", "github|u1")}).Label(); got != "Cursor User (u1)" {
		t.Fatalf("label = %q", got)
	}
	if meta := BuildMetadata(nil); meta["type"] != "cursor" {
		t.Fatalf("nil bundle metadata: %v", meta)
	}
}
