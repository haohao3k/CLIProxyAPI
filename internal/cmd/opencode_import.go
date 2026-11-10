// Package cmd contains CLI helpers. This file implements importing an OpenCode
// Go API key into the auth store as a first-class "opencode" credential.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/ojson"
	opencodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// OpenCodeImportOptions controls the opencode-import flow.
type OpenCodeImportOptions struct {
	// APIKey, when non-empty, overrides discovery.
	APIKey string
	// AuthFilePath overrides the default ~/.local/share/opencode/auth.json lookup path.
	AuthFilePath string
	// Label customizes the saved file name; defaults to "default".
	Label string
}

const opencodeKeyHint = "pass -opencode-api-key, set OPENCODE_API_KEY, or sign in with the OpenCode CLI (~/.local/share/opencode/auth.json)"

// DoOpenCodeImport reads an OpenCode Go API key and persists it as an "opencode"
// provider credential.
//
// Discovery order: APIKey option > OPENCODE_API_KEY / OPENCODE_GO_API_KEY env var >
// OpenCode CLI auth file (~/.local/share/opencode/auth.json or $XDG_DATA_HOME/opencode/auth.json).
func DoOpenCodeImport(cfg *config.Config, opts *OpenCodeImportOptions) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if resolved, errResolve := util.ResolveAuthDir(cfg.AuthDir); errResolve == nil {
		cfg.AuthDir = resolved
	}
	if opts == nil {
		opts = &OpenCodeImportOptions{}
	}

	apiKey, source, errDiscover := discoverOpenCodeAPIKey(opts)
	if errDiscover != nil {
		log.Errorf("opencode-import: %v", errDiscover)
		return
	}

	authDir := strings.TrimSpace(cfg.AuthDir)
	if authDir == "" {
		log.Error("opencode-import: auth directory is empty; cannot save credential")
		return
	}

	label := sanitizeFilePart(opts.Label)
	if label == "" {
		label = "default"
	}
	fileName := fmt.Sprintf("opencode-%s.json", label)

	metadata := map[string]any{
		"type":      opencodeauth.ProviderKey,
		"api_key":   apiKey,
		"base_url":  opencodeauth.DefaultBaseURL,
		"auth_kind": "apikey",
		"label":     label,
	}
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: opencodeauth.ProviderKey,
		FileName: fileName,
		Label:    label,
		Metadata: metadata,
		Attributes: map[string]string{
			"api_key":   apiKey,
			"base_url":  opencodeauth.DefaultBaseURL,
			"auth_kind": "apikey",
		},
	}

	store := sdkAuth.GetTokenStore()
	if setter, ok := store.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(authDir)
	}
	path, errSave := store.Save(context.Background(), record)
	if errSave != nil {
		log.Errorf("opencode-import: save credential failed: %v", errSave)
		return
	}

	fmt.Printf("OpenCode Go API key imported from %s\n", source)
	fmt.Printf("Credential saved to %s\n", path)
	fmt.Println("Models served through the OpenCode Go provider:")
	for _, model := range registry.GetOpencodeModels() {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		fmt.Printf("  - %s\n", model.ID)
	}
}

func discoverOpenCodeAPIKey(opts *OpenCodeImportOptions) (apiKey string, source string, err error) {
	if opts != nil {
		if key := strings.TrimSpace(opts.APIKey); key != "" {
			return key, "-opencode-api-key flag", nil
		}
	}
	for _, envName := range []string{"OPENCODE_API_KEY", "OPENCODE_GO_API_KEY"} {
		if key := strings.TrimSpace(os.Getenv(envName)); key != "" {
			return key, envName + " env var", nil
		}
	}

	paths, errPaths := resolveOpenCodeAuthFilePaths(opts)
	if errPaths != nil {
		return "", "", errPaths
	}

	var lastErr error
	for _, p := range paths {
		raw, errRead := os.ReadFile(p)
		if errRead != nil {
			lastErr = errRead
			continue
		}
		if key := extractOpenCodeKeyFromAuthJSON(raw); key != "" {
			return key, p, nil
		}
		lastErr = fmt.Errorf("no OpenCode API key found in %s", p)
	}
	if lastErr != nil {
		return "", "", fmt.Errorf("%w; %s", lastErr, opencodeKeyHint)
	}
	return "", "", fmt.Errorf("no OpenCode API key found; %s", opencodeKeyHint)
}

func resolveOpenCodeAuthFilePaths(opts *OpenCodeImportOptions) ([]string, error) {
	if opts != nil && strings.TrimSpace(opts.AuthFilePath) != "" {
		expanded, errExpand := expandHomeDir(strings.TrimSpace(opts.AuthFilePath))
		if errExpand != nil {
			return nil, errExpand
		}
		return []string{expanded}, nil
	}

	var candidates []string
	if xdgData := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdgData != "" {
		candidates = append(candidates, filepath.Join(xdgData, "opencode", "auth.json"))
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		if len(candidates) > 0 {
			return candidates, nil
		}
		return nil, fmt.Errorf("resolve home dir: %w", errHome)
	}
	candidates = append(candidates,
		filepath.Join(home, ".local", "share", "opencode", "auth.json"),
		filepath.Join(home, ".config", "opencode", "auth.json"),
	)
	return candidates, nil
}

func extractOpenCodeKeyFromAuthJSON(raw []byte) string {
	paths := []string{
		"opencode.key",
		"opencode.api_key",
		"opencode.apiKey",
		"opencode.access_token",
		"opencode-go.key",
		"opencode-go.api_key",
		"opencode-go.apiKey",
		"zen.key",
		"zen.api_key",
		"api_key",
		"apiKey",
		"key",
		"access_token",
	}
	for _, p := range paths {
		if val := strings.TrimSpace(ojson.GetBytesNoCopy(raw, p).String()); val != "" {
			return val
		}
	}
	return ""
}
