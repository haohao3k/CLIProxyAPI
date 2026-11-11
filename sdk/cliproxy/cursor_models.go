package cliproxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cursorModelFetchTimeout bounds how long the proxy waits for Cursor's
// AvailableModels endpoint.
const cursorModelFetchTimeout = 15 * time.Second

// fetchCursorModelsForAuth asks Cursor's AIServer which models the account can
// use. On failure it returns nil so the caller can fall back to the static
// catalog.
func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	token := cursorTokenFromAuth(auth)
	if token == "" {
		return nil
	}

	client := helps.NewProxyAwareHTTPClient(ctx, nil, auth, cursorModelFetchTimeout)
	if s != nil && s.cfg != nil {
		client = helps.NewProxyAwareHTTPClient(ctx, s.cfg, auth, cursorModelFetchTimeout)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorwire.AvailableModelsEndpoint, bytes.NewReader(nil))
	if err != nil {
		log.Debugf("cursor model fetch: build request: %v", err)
		return nil
	}
	cursorwire.ApplyHeaders(req, token, false)

	resp, err := client.Do(req)
	if err != nil {
		log.Debugf("cursor model fetch: do request: %v", err)
		return nil
	}
	body, err := cursorwire.DecodeMaybeGzip(resp.Body)
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("cursor model fetch: close body: %v", errClose)
	}
	if err != nil {
		log.Debugf("cursor model fetch: read body: %v", err)
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("cursor model fetch: status %d: %s", resp.StatusCode, string(body[:min(len(body), 256)]))
		return nil
	}

	names, err := cursorwire.DecodeAvailableModels(body)
	if err != nil {
		log.Debugf("cursor model fetch: decode models: %v", err)
		return nil
	}
	if len(names) == 0 {
		return nil
	}

	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		id := strings.TrimSpace(name)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		displayName := id
		if strings.HasPrefix(strings.ToLower(id), "cursor-") {
			displayName = id
		} else {
			id = "cursor-" + id
			displayName = id
		}
		models = append(models, &registry.ModelInfo{
			ID:                        id,
			Object:                    "model",
			Created:                   now,
			OwnedBy:                   "cursor",
			Type:                      "cursor",
			DisplayName:               displayName,
			ContextLength:             200000,
			MaxCompletionTokens:       64000,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
		})
	}
	return models
}

// cursorTokenFromAuth extracts the Cursor access token from auth metadata or
// attributes, mirroring the executor credential lookup.
func cursorTokenFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"access_token", "cookie"} {
			if v, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"access_token", "cookie", "api_key"} {
			if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
				return v
			}
		}
	}
	return ""
}

// mergeCursorModels combines live-fetched models with the static catalog,
// keeping the live order and augmenting any missing static entries.
func mergeCursorModels(live, static []*registry.ModelInfo) []*registry.ModelInfo {
	if len(live) == 0 {
		return static
	}
	staticByID := make(map[string]*registry.ModelInfo, len(static))
	for _, m := range static {
		if m == nil {
			continue
		}
		staticByID[strings.ToLower(m.ID)] = m
	}

	out := make([]*registry.ModelInfo, 0, len(live)+len(static))
	seen := make(map[string]struct{}, len(live)+len(static))
	for _, m := range live {
		if m == nil {
			continue
		}
		key := strings.ToLower(m.ID)
		seen[key] = struct{}{}
		if enriched, ok := staticByID[key]; ok {
			clone := *enriched
			if m.DisplayName != "" {
				clone.DisplayName = m.DisplayName
			}
			out = append(out, &clone)
			continue
		}
		out = append(out, m)
	}
	for _, m := range static {
		if m == nil {
			continue
		}
		key := strings.ToLower(m.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		clone := *m
		out = append(out, &clone)
	}
	return out
}

// cursorLiveModelsEnabled reports whether live Cursor model loading is enabled
// for the given auth. It is enabled by default for all Cursor credentials so
// the account's full AvailableModels list is automatically fetched from Cursor
// AIServer; set "live_models": false in the auth file to opt out.
func (s *Service) cursorLiveModelsEnabled(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if enabled, ok := parseCursorLiveModelsFlag(auth.Attributes["live_models"]); ok {
			return enabled
		}
	}
	if auth.Metadata != nil {
		switch v := auth.Metadata["live_models"].(type) {
		case bool:
			return v
		case string:
			if enabled, ok := parseCursorLiveModelsFlag(v); ok {
				return enabled
			}
		}
	}
	return true
}

func parseCursorLiveModelsFlag(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}
