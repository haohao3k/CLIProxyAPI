// Package opencode provides authentication, credential normalization, and token
// management for OpenCode Go (Zen Go gateway).
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// ProviderKey is the canonical provider identifier for OpenCode Go credentials.
	ProviderKey = "opencode"

	// DefaultBaseURL is the default OpenCode Go gateway base URL.
	DefaultBaseURL = "https://opencode.ai/zen/go/v1"

	// OpenCodeGoAPIBaseURL aliases DefaultBaseURL for compatibility.
	OpenCodeGoAPIBaseURL = DefaultBaseURL

	// DefaultAnthropicBaseURL is the Anthropic-compatible base URL on the OpenCode Go gateway.
	DefaultAnthropicBaseURL = "https://opencode.ai/zen/go"

	// DefaultUsageURL is the OpenCode Go usage endpoint URL.
	DefaultUsageURL = "https://opencode.ai/zen/go/v1/usage"

	// OpenCodeGoAuthURL is where users mint an OpenCode Go API key after subscribing.
	OpenCodeGoAuthURL = "https://opencode.ai/auth"

	// OpenCodeGoUsagePath is the first-party usage endpoint path relative to the v1 base URL.
	OpenCodeGoUsagePath = "/usage"

	// OpenCodeGoUserAgent identifies CPA requests to the OpenCode Go gateway.
	OpenCodeGoUserAgent = "cli-proxy-api"

	// DefaultLabel is the default human-readable label for OpenCode Go credentials.
	DefaultLabel = "OpenCode Go"

	httpClientTimeout = 30 * time.Second
)

// IsOpencodeProvider reports whether provider is the canonical OpenCode Go provider
// key or one of its supported aliases ("opencode", "opencode-go", "ocg", "opencode_go").
func IsOpencodeProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "opencode", "opencode-go", "ocg", "opencode_go":
		return true
	default:
		return false
	}
}

// CanonicalProvider normalizes OpenCode Go provider aliases to ProviderKey ("opencode").
// Non-OpenCode provider strings are returned trimmed and lowercased.
func CanonicalProvider(provider string) string {
	if IsOpencodeProvider(provider) {
		return ProviderKey
	}
	return strings.ToLower(strings.TrimSpace(provider))
}

// OpenCodeAuth performs OpenCode Go API key validation.
type OpenCodeAuth struct {
	httpClient *http.Client
}

// NewOpenCodeAuth creates an OpenCode helper using config proxy settings.
func NewOpenCodeAuth(cfg *config.Config) *OpenCodeAuth {
	return NewOpenCodeAuthWithProxyURL(cfg, "")
}

// NewOpenCodeAuthWithProxyURL creates an OpenCode helper with an explicit proxy URL.
func NewOpenCodeAuthWithProxyURL(cfg *config.Config, proxyURL string) *OpenCodeAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &OpenCodeAuth{httpClient: util.SetProxy(&sdkCfg, &http.Client{Timeout: httpClientTimeout})}
}

// NormalizeBaseURL resolves the gateway base URL, tolerating a missing /v1 suffix.
func NormalizeBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	if strings.HasSuffix(strings.ToLower(trimmed), "/v1") {
		return trimmed
	}
	return trimmed + "/v1"
}

// UsageURL resolves the usage endpoint for a base URL.
func UsageURL(baseURL string) string {
	return strings.TrimRight(NormalizeBaseURL(baseURL), "/") + OpenCodeGoUsagePath
}

// ValidateKey checks an API key against the gateway models endpoint.
func (a *OpenCodeAuth) ValidateKey(ctx context.Context, baseURL, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return fmt.Errorf("opencode: api key is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	url := strings.TrimRight(NormalizeBaseURL(baseURL), "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("opencode: create validation request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", OpenCodeGoUserAgent)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: validation request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("opencode: close validation body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("opencode: read validation response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("opencode: api key rejected (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode: validation failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("opencode: parse validation response: %w", err)
	}
	if len(payload.Data) == 0 {
		return fmt.Errorf("opencode: gateway returned no models")
	}
	return nil
}

// OpenCodeCreds extracts the OpenCode Go API key from auth metadata/attributes.
func OpenCodeCreds(metadata map[string]any, attributes map[string]string) string {
	if metadata != nil {
		for _, key := range []string{"api_key", "api-key", "apiKey", "access_token", "accessToken", "apikey", "token"} {
			if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if attributes != nil {
		for _, key := range []string{"api_key", "access_token", "token"} {
			if v := strings.TrimSpace(attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

// ResolveBaseURL extracts an explicit gateway base URL override, if any.
func ResolveBaseURL(metadata map[string]any, attributes map[string]string) string {
	if metadata != nil {
		for _, key := range []string{"base_url", "base-url", "baseUrl"} {
			if v, ok := metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if attributes != nil {
		for _, key := range []string{"base_url", "base-url"} {
			if v := strings.TrimSpace(attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

// ApplyAuthFileAttributes populates the attributes that OpenAICompatExecutor and
// management API tools read from an Auth record when loading a "type":"opencode"
// (or "opencode-go" / "ocg") credential file from disk.
func ApplyAuthFileAttributes(auth *cliproxyauth.Auth, metadata map[string]any) {
	if auth == nil {
		return
	}
	auth.Provider = ProviderKey
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}

	rawBaseURL := ResolveBaseURL(metadata, auth.Attributes)
	auth.Attributes["base_url"] = NormalizeBaseURL(rawBaseURL)

	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		if apiKey := OpenCodeCreds(metadata, auth.Attributes); apiKey != "" {
			auth.Attributes["api_key"] = apiKey
		}
	}
	if strings.TrimSpace(auth.Attributes["auth_kind"]) == "" {
		if metadata != nil {
			if kind, ok := metadata["auth_kind"].(string); ok && strings.TrimSpace(kind) != "" {
				auth.Attributes["auth_kind"] = strings.TrimSpace(kind)
			}
		}
		if strings.TrimSpace(auth.Attributes["auth_kind"]) == "" {
			auth.Attributes["auth_kind"] = "apikey"
		}
	}
	if strings.TrimSpace(auth.Label) == "" {
		if metadata != nil {
			if label, ok := metadata["label"].(string); ok && strings.TrimSpace(label) != "" {
				auth.Label = strings.TrimSpace(label)
			}
		}
		if strings.TrimSpace(auth.Label) == "" {
			auth.Label = DefaultLabel
		}
	}
}
