package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func synthesizeSingleZAIFile(t *testing.T, payload map[string]any) *coreauth.Auth {
	t.Helper()
	tempDir := t.TempDir()
	data, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(tempDir, "zai-test.json"), data, 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	auths, err := NewFileSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	return auths[0]
}

func TestFileSynthesizer_ZAIAPIKeyFile(t *testing.T) {
	auth := synthesizeSingleZAIFile(t, map[string]any{
		"type":    "zai",
		"api_key": "abc.def",
	})
	if auth.Provider != "zai" {
		t.Fatalf("provider = %q", auth.Provider)
	}
	if got := auth.Attributes["base_url"]; got != "https://api.z.ai/api/coding/paas/v4" {
		t.Fatalf("base_url = %q", got)
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "abc.def" {
		t.Fatalf("api_key = %q", got)
	}
	// File-backed credentials are classified as oauth by the synthesizer,
	// matching every other auth-file provider.
	if kind := auth.AuthKind(); kind != coreauth.AuthKindOAuth {
		t.Fatalf("auth kind = %q, want oauth", kind)
	}
}

func TestFileSynthesizer_ZAIOAuthFile(t *testing.T) {
	auth := synthesizeSingleZAIFile(t, map[string]any{
		"type":      "zai",
		"auth_kind": "oauth",
		"api_key":   "minted.secret",
		"base_url":  "https://api.z.ai/api/coding/paas/v4/",
		"email":     "dev@example.com",
	})
	if got := auth.Attributes["base_url"]; got != "https://api.z.ai/api/coding/paas/v4" {
		t.Fatalf("base_url = %q (trailing slash should be trimmed)", got)
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "minted.secret" {
		t.Fatalf("api_key = %q", got)
	}
	if kind := auth.AuthKind(); kind != coreauth.AuthKindOAuth {
		t.Fatalf("auth kind = %q, want oauth", kind)
	}
	if auth.Label != "dev@example.com" {
		t.Fatalf("label = %q", auth.Label)
	}
}
