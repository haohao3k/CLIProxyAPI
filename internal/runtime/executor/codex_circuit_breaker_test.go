package executor

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexCircuitBreakerDisablesThroughManagerAndRecovers(t *testing.T) {
	ctx := context.Background()
	m := cliproxyauth.NewManager(nil, nil, nil)
	auth := &cliproxyauth.Auth{ID: "codex-cb-acct", Provider: "codex", Label: "cb@example.com", Metadata: map[string]any{"email": "cb@example.com"}}
	if _, err := m.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	SetCodexCircuitBreakerHost(m)
	t.Cleanup(func() { SetCodexCircuitBreakerHost(nil) })

	minRemaining := 30
	cfg := &config.Config{}
	cfg.Codex.QuotaCircuitBreakerMinRemaining = &minRemaining

	before := time.Now()
	hdr := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
	}
	observeCodexQuotaCircuitBreakerWithMeta(ctx, cfg, auth, hdr, "sess-cb", "gpt-6-astra")

	got, _ := m.GetByID(auth.ID)
	if !got.Disabled {
		t.Fatal("breaker must disable the credential in the auth manager")
	}
	at, ok := cliproxyauth.CircuitBreakerReenableAt(got)
	if !ok || at.Before(before.Add(time.Hour-time.Second)) || at.After(time.Now().Add(time.Hour+time.Second)) {
		t.Fatalf("reenable-at = %v, %v; want ~now+1h", at, ok)
	}
	if !codexCircuitBreakerActive(auth.ID) {
		t.Fatal("telemetry must report the breaker as active")
	}
	// The stale request-scoped clone is still enabled; the check must read the manager.
	if err := checkCodexCircuitBreakerTripped(auth); err == nil {
		t.Fatal("queued request on a tripped credential must be rejected")
	}

	m.ReenableExpiredCircuitBreakers(ctx, at.Add(time.Second))
	if err := checkCodexCircuitBreakerTripped(auth); err != nil {
		t.Fatalf("credential must be usable after the quota window resets: %v", err)
	}
	if codexCircuitBreakerActive(auth.ID) {
		t.Fatal("breaker must be inactive after reset")
	}
}
