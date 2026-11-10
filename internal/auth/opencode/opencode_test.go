package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestIsOpencodeProviderAndNormalizeBaseURL(t *testing.T) {
	for _, alias := range []string{"opencode", "opencode-go", "ocg", "opencode_go", " OpenCode-Go "} {
		if !IsOpencodeProvider(alias) {
			t.Fatalf("expected IsOpencodeProvider(%q) = true", alias)
		}
		if got := CanonicalProvider(alias); got != ProviderKey {
			t.Fatalf("CanonicalProvider(%q) = %q, want %q", alias, got, ProviderKey)
		}
	}
	if IsOpencodeProvider("mistral") {
		t.Fatal("expected IsOpencodeProvider(mistral) = false")
	}

	if got := NormalizeBaseURL(""); got != DefaultBaseURL {
		t.Fatalf("NormalizeBaseURL(\"\") = %q, want %q", got, DefaultBaseURL)
	}
	if got := NormalizeBaseURL("https://opencode.ai/zen/go"); got != DefaultBaseURL {
		t.Fatalf("NormalizeBaseURL without /v1 = %q, want %q", got, DefaultBaseURL)
	}
	if got := UsageURL(""); got != DefaultUsageURL {
		t.Fatalf("UsageURL(\"\") = %q, want %q", got, DefaultUsageURL)
	}
}

func TestApplyAuthFileAttributes(t *testing.T) {
	record := &cliproxyauth.Auth{
		ID:       "opencode-test.json",
		Provider: "opencode-go",
	}
	meta := map[string]any{
		"type":    "opencode-go",
		"api_key": "oc-test-key",
	}
	ApplyAuthFileAttributes(record, meta)

	if record.Provider != ProviderKey {
		t.Fatalf("Provider = %q, want %q", record.Provider, ProviderKey)
	}
	if got := record.Attributes["api_key"]; got != "oc-test-key" {
		t.Fatalf("Attributes[api_key] = %q, want %q", got, "oc-test-key")
	}
	if got := record.Attributes["base_url"]; got != DefaultBaseURL {
		t.Fatalf("Attributes[base_url] = %q, want %q", got, DefaultBaseURL)
	}
	if got := record.Attributes["auth_kind"]; got != "apikey" {
		t.Fatalf("Attributes[auth_kind] = %q, want apikey", got)
	}
	if record.Label != DefaultLabel {
		t.Fatalf("Label = %q, want %q", record.Label, DefaultLabel)
	}
}

func TestValidateKeyAndTokenStorage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer oc-valid-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"}]}`))
	}))
	defer srv.Close()

	helper := NewOpenCodeAuth(nil)
	baseURL := srv.URL + "/zen/go/v1"
	if err := helper.ValidateKey(context.Background(), baseURL, "oc-valid-key"); err != nil {
		t.Fatalf("ValidateKey valid key error: %v", err)
	}
	if err := helper.ValidateKey(context.Background(), baseURL, "oc-bad-key"); err == nil {
		t.Fatal("expected ValidateKey to fail for invalid key")
	}

	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, CredentialFileName("oc-valid-key"))
	ts := &TokenStorage{
		APIKey: "oc-valid-key",
		Label:  "My OpenCode",
	}
	if err := ts.SaveTokenToFile(outFile); err != nil {
		t.Fatalf("SaveTokenToFile error: %v", err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("expected non-empty token file")
	}
}
