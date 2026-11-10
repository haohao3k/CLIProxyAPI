package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
)

func TestDoOpenCodeImportFromFlagAndAuthFile(t *testing.T) {
	authDir := t.TempDir()
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())
	cfg := &config.Config{AuthDir: authDir}

	DoOpenCodeImport(cfg, &OpenCodeImportOptions{
		APIKey: "oc-flag-key-123",
		Label:  "work",
	})

	savedPath := filepath.Join(authDir, "opencode-work.json")
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read saved opencode auth file: %v", err)
	}
	if got := ojson.GetBytesNoCopy(raw, "type").String(); got != "opencode" {
		t.Fatalf("saved type = %q, want opencode", got)
	}
	if got := ojson.GetBytesNoCopy(raw, "api_key").String(); got != "oc-flag-key-123" {
		t.Fatalf("saved api_key = %q, want oc-flag-key-123", got)
	}

	// Test importing from OpenCode CLI auth.json
	cliAuthFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(cliAuthFile, []byte(`{"opencode":{"type":"api","key":"oc-cli-key-456"}}`), 0o600); err != nil {
		t.Fatalf("write cli auth.json: %v", err)
	}
	DoOpenCodeImport(cfg, &OpenCodeImportOptions{
		AuthFilePath: cliAuthFile,
	})
	defaultPath := filepath.Join(authDir, "opencode-default.json")
	rawDefault, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatalf("read default opencode auth file: %v", err)
	}
	if got := ojson.GetBytesNoCopy(rawDefault, "api_key").String(); got != "oc-cli-key-456" {
		t.Fatalf("saved api_key from auth.json = %q, want oc-cli-key-456", got)
	}
}
