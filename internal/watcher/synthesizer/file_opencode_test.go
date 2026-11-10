package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestFileSynthesizer_Synthesize_OpencodeAliases(t *testing.T) {
	tempDir := t.TempDir()

	files := map[string]map[string]any{
		"opencode-main.json": {
			"type":    "opencode",
			"api_key": "oc-key-1",
		},
		"opencode-go.json": {
			"type":     "opencode-go",
			"api_key":  "oc-key-2",
			"base_url": "https://custom.opencode.ai/zen/go/v1/",
		},
		"ocg.json": {
			"type":   "ocg",
			"apiKey": "oc-key-3",
		},
	}

	for name, payload := range files {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(tempDir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(auths) != 3 {
		t.Fatalf("len(auths) = %d, want 3", len(auths))
	}

	byFile := make(map[string]map[string]string, len(auths))
	for _, a := range auths {
		if a.Provider != opencode.ProviderKey {
			t.Fatalf("auth %s Provider = %q, want %q", a.FileName, a.Provider, opencode.ProviderKey)
		}
		byFile[a.FileName] = a.Attributes
	}

	if got := byFile["opencode-main.json"]["api_key"]; got != "oc-key-1" {
		t.Fatalf("opencode-main api_key = %q, want oc-key-1", got)
	}
	if got := byFile["opencode-main.json"]["base_url"]; got != opencode.DefaultBaseURL {
		t.Fatalf("opencode-main base_url = %q, want %q", got, opencode.DefaultBaseURL)
	}
	if got := byFile["opencode-go.json"]["base_url"]; got != "https://custom.opencode.ai/zen/go/v1" {
		t.Fatalf("opencode-go base_url = %q, want https://custom.opencode.ai/zen/go/v1", got)
	}
	if got := byFile["ocg.json"]["api_key"]; got != "oc-key-3" {
		t.Fatalf("ocg api_key = %q, want oc-key-3", got)
	}
}
