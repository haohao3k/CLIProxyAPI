package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelexposure"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"golang.org/x/net/context"
)

// ListModels returns the models list for handlerType ("openai", "claude" or "gemini") as the
// caller in ctx may see it: model exposure names and filters, Cursor aliases, combos, and the
// caller key's policy. Gemini names cannot carry a provider prefix, so they stay bare there.
func (h *BaseAPIHandler) ListModels(ctx context.Context, handlerType string) []map[string]any {
	return h.listModels(ctx, handlerType, handlerType != "gemini")
}

// ListModelsUnprefixed is ListModels with registry ids, for catalogs keyed by registry id.
func (h *BaseAPIHandler) ListModelsUnprefixed(ctx context.Context, handlerType string) []map[string]any {
	return h.listModels(ctx, handlerType, false)
}

func (h *BaseAPIHandler) listModels(ctx context.Context, handlerType string, prefixIDs bool) []map[string]any {
	reg := registry.GetGlobalRegistry()
	base := reg.GetAvailableModels(handlerType)
	cfg := h.CurrentConfig()
	if cfg == nil {
		return h.AppendComboModels(handlerType, base)
	}
	exposure := &cfg.ModelExposure
	apiKey := ""
	if ctx != nil {
		apiKey = coreusage.APIKeyFromContext(ctx)
	}
	policy := modelexposure.PolicyFor(cfg.APIKeyPolicies, apiKey)
	if !exposure.Enabled && !policy.Restricted() && len(cfg.CursorModelAliases) == 0 {
		return h.AppendComboModels(handlerType, base)
	}
	prefixIDs = prefixIDs && exposure.Enabled
	listBare := !exposure.Enabled || !exposure.ListPrefixedOnly || !prefixIDs

	out := make([]map[string]any, 0, len(base))
	seen := make(map[string]struct{}, len(base))
	add := func(model map[string]any, id string) {
		key := strings.ToLower(id)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, withModelID(model, handlerType, id))
	}
	for _, model := range base {
		id := modelMapID(model, handlerType)
		if id == "" {
			continue
		}
		anyVisible := false
		for _, provider := range reg.GetModelProviders(id) {
			key := modelexposure.ProviderKey(provider)
			if ok, _ := modelexposure.DecideWithAliases(exposure, cfg.CursorModelAliases, key, id); !ok {
				continue
			}
			exposedID := modelexposure.ExposedID(exposure, key, id)
			if !policy.AllowsName(exposedID) {
				continue
			}
			anyVisible = true
			if prefixIDs {
				add(model, exposedID)
			}
		}
		if anyVisible && listBare {
			add(model, id)
		}
	}
	if hasRegisteredCursorProvider() {
		cursorPrefix := modelexposure.Settings(exposure, "cursor").Prefix
		for _, alias := range cfg.CursorModelAliases {
			exposedID := cursorPrefix + "/" + alias.Alias
			if !policy.AllowsName(exposedID) {
				continue
			}
			if prefixIDs {
				add(aliasModelMap(alias, handlerType), exposedID)
			}
			if listBare {
				add(aliasModelMap(alias, handlerType), alias.Alias)
			}
		}
	}
	withCombos := h.AppendComboModels(handlerType, out)
	filtered := append(make([]map[string]any, 0, len(withCombos)), withCombos[:len(out)]...)
	for _, combo := range withCombos[len(out):] {
		if policy.AllowsCombo(modelMapID(combo, handlerType)) {
			filtered = append(filtered, combo)
		}
	}
	return filtered
}

func modelMapID(model map[string]any, handlerType string) string {
	if handlerType == "gemini" {
		if name, ok := model["name"].(string); ok {
			return strings.TrimPrefix(strings.TrimSpace(name), "models/")
		}
	}
	if id, ok := model["id"].(string); ok {
		return strings.TrimSpace(id)
	}
	return ""
}

func withModelID(model map[string]any, handlerType, id string) map[string]any {
	copied := make(map[string]any, len(model))
	for k, v := range model {
		copied[k] = v
	}
	if handlerType == "gemini" {
		copied["name"] = "models/" + id
	} else {
		copied["id"] = id
	}
	return copied
}

func aliasModelMap(alias config.CursorModelAlias, handlerType string) map[string]any {
	switch handlerType {
	case "gemini":
		return map[string]any{
			"displayName":                alias.Alias,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		}
	case "claude":
		return map[string]any{
			"type":         "model",
			"object":       "model",
			"display_name": alias.Alias,
			"created_at":   "2026-01-01T00:00:00Z",
			"owned_by":     "cursor",
		}
	default:
		return map[string]any{
			"object":   "model",
			"created":  int64(1735689600),
			"owned_by": "cursor",
		}
	}
}

// CallerContext returns the request context that carries the caller's client key, or nil.
func CallerContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return nil
	}
	return c.Request.Context()
}
