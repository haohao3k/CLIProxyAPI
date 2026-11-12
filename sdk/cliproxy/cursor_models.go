package cliproxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cursorwire "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cursorModelFetchTimeout bounds how long the proxy waits for Cursor's
// GetUsableModels / AvailableModels endpoints.
const cursorModelFetchTimeout = 15 * time.Second

// fetchCursorModelsForAuth asks Cursor's AgentService which models the account
// can use via GetUsableModels (falling back to legacy AvailableModels only if needed).
// On failure it returns nil so the caller can fall back to the static catalog.
func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	token := cursorTokenFromAuth(auth)
	if token == "" {
		return nil
	}
	machineID := cursorMachineIDFromAuth(auth)

	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	client := helps.NewDevinHTTPClient(ctx, cfg, auth, cursorModelFetchTimeout)

	if usable := fetchCursorUsableModels(ctx, client, token, machineID); len(usable) > 0 {
		cursorwire.RecordUsableModels(usable)
		return buildCursorModelsFromUsable(usable)
	}

	return fetchCursorLegacyAvailableModels(ctx, client, token)
}

func fetchCursorUsableModels(ctx context.Context, client *http.Client, token, machineID string) []cursorwire.UsableModel {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorwire.GetUsableModelsEndpoint, bytes.NewReader(nil))
	if err != nil {
		log.Debugf("cursor model fetch (agent): build request: %v", err)
		return nil
	}
	cursorwire.ApplyAgentHeaders(req, token, machineID, false)

	resp, err := client.Do(req)
	if err != nil {
		log.Debugf("cursor model fetch (agent): do request: %v", err)
		return nil
	}
	body, err := cursorwire.DecodeMaybeGzip(resp.Body)
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("cursor model fetch (agent): close body: %v", errClose)
	}
	if err != nil {
		log.Debugf("cursor model fetch (agent): read body: %v", err)
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("cursor model fetch (agent): status %d: %s", resp.StatusCode, string(body[:min(len(body), 256)]))
		return nil
	}
	usable, err := cursorwire.DecodeUsableModels(body)
	if err != nil {
		log.Debugf("cursor model fetch (agent): decode models: %v", err)
		return nil
	}
	return usable
}

func buildCursorModelsFromUsable(usable []cursorwire.UsableModel) []*registry.ModelInfo {
	now := time.Now().Unix()
	synthesized := cursorwire.SynthesizeFamilyModels(usable)
	models := make([]*registry.ModelInfo, 0, len(usable)+len(synthesized)+1)
	seen := make(map[string]struct{}, len(usable)+len(synthesized)+1)

	addModel := func(m *registry.ModelInfo) {
		if m == nil || strings.TrimSpace(m.ID) == "" {
			return
		}
		key := strings.ToLower(strings.TrimSpace(m.ID))
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		models = append(models, m)
	}

	for _, u := range usable {
		rawID := strings.TrimSpace(u.ID)
		if rawID == "" {
			continue
		}
		clientID := cursorwire.ClientModelID(rawID)
		displayName := strings.TrimSpace(u.DisplayName)
		if displayName == "" {
			displayName = clientID
		}
		var thinkingSupport *registry.ThinkingSupport
		if levels, zeroAllowed := cursorwire.FamilyThinkingLevels(rawID); len(levels) > 0 {
			thinkingSupport = &registry.ThinkingSupport{
				ZeroAllowed: zeroAllowed,
				Levels:      levels,
			}
		}
		addModel(&registry.ModelInfo{
			ID:                        clientID,
			Name:                      rawID,
			Object:                    "model",
			Created:                   now,
			OwnedBy:                   "cursor",
			Type:                      "cursor",
			DisplayName:               displayName,
			ContextLength:             200000,
			MaxCompletionTokens:       64000,
			Thinking:                  thinkingSupport,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
		})
		if strings.EqualFold(rawID, "default") {
			addModel(&registry.ModelInfo{
				ID:                        "cursor-auto",
				Name:                      "default",
				Object:                    "model",
				Created:                   now,
				OwnedBy:                   "cursor",
				Type:                      "cursor",
				DisplayName:               "Cursor Auto",
				ContextLength:             200000,
				MaxCompletionTokens:       64000,
				SupportedInputModalities:  []string{"text", "image"},
				SupportedOutputModalities: []string{"text"},
			})
		}
	}

	for _, fam := range synthesized {
		clientID := cursorwire.ClientModelID(fam.ID)
		displayName := strings.TrimSpace(fam.DisplayName)
		if displayName == "" {
			displayName = clientID
		}
		addModel(&registry.ModelInfo{
			ID:                  clientID,
			Name:                fam.ID,
			Object:              "model",
			Created:             now,
			OwnedBy:             "cursor",
			Type:                "cursor",
			DisplayName:         displayName,
			ContextLength:       200000,
			MaxCompletionTokens: 64000,
			Thinking: &registry.ThinkingSupport{
				ZeroAllowed: fam.ZeroAllowed,
				Levels:      fam.Levels,
			},
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
		})
	}

	return models
}

func fetchCursorLegacyAvailableModels(ctx context.Context, client *http.Client, token string) []*registry.ModelInfo {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorwire.AvailableModelsEndpoint, bytes.NewReader(nil))
	if err != nil {
		log.Debugf("cursor model fetch (legacy): build request: %v", err)
		return nil
	}
	cursorwire.ApplyHeaders(req, token, false)

	resp, err := client.Do(req)
	if err != nil {
		log.Debugf("cursor model fetch (legacy): do request: %v", err)
		return nil
	}
	body, err := cursorwire.DecodeMaybeGzip(resp.Body)
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("cursor model fetch (legacy): close body: %v", errClose)
	}
	if err != nil {
		log.Debugf("cursor model fetch (legacy): read body: %v", err)
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("cursor model fetch (legacy): status %d: %s", resp.StatusCode, string(body[:min(len(body), 256)]))
		return nil
	}

	names, err := cursorwire.DecodeAvailableModels(body)
	if err != nil || len(names) == 0 {
		return nil
	}
	usable := make([]cursorwire.UsableModel, 0, len(names))
	for _, n := range names {
		if id := strings.TrimSpace(n); id != "" {
			usable = append(usable, cursorwire.UsableModel{ID: id, DisplayName: cursorwire.ClientModelID(id)})
		}
	}
	cursorwire.RecordUsableModels(usable)
	return buildCursorModelsFromUsable(usable)
}

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

func cursorMachineIDFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range []string{"machine_id", "machineId"} {
			if v, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"machine_id", "machineId"} {
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
			if clone.Thinking == nil && m.Thinking != nil {
				clone.Thinking = m.Thinking
			}
			if clone.Name == "" && m.Name != "" {
				clone.Name = m.Name
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
		if clone.Thinking == nil {
			if levels, zeroAllowed := cursorwire.FamilyThinkingLevels(clone.ID); len(levels) > 0 {
				clone.Thinking = &registry.ThinkingSupport{
					ZeroAllowed: zeroAllowed,
					Levels:      levels,
				}
			}
		}
		out = append(out, &clone)
	}
	return out
}

// cursorLiveModelsEnabled reports whether live Cursor model loading is enabled
// for the given auth. Enabled by default unless "live_models": false is set.
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
