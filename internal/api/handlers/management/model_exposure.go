package management

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelexposure"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	modelExposureField      = "model-exposure"
	cursorModelAliasesField = "cursor-model-aliases"
	keyPoliciesField        = "key-policies"
)

// GetModelExposure returns the model-exposure section (`GET /v0/management/model-exposure`).
func (h *Handler) GetModelExposure(c *gin.Context) {
	h.mu.Lock()
	exposure := h.cfg.ModelExposure
	h.mu.Unlock()
	if exposure.Providers == nil {
		exposure.Providers = map[string]config.ModelExposureProvider{}
	}
	c.JSON(200, gin.H{modelExposureField: exposure})
}

// PutModelExposure replaces the model-exposure section (`PUT /v0/management/model-exposure`).
func (h *Handler) PutModelExposure(c *gin.Context) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	raw, ok := body[modelExposureField]
	if !ok {
		c.JSON(400, gin.H{"error": "missing " + modelExposureField})
		return
	}
	var exposure config.ModelExposureConfig
	if err := json.Unmarshal(raw, &exposure); err != nil {
		c.JSON(400, gin.H{"error": "invalid " + modelExposureField})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ModelExposure = exposure
	h.cfg.SanitizeModelExposure()
	h.persistLocked(c)
}

// GetCursorModelAliases returns the Cursor effort aliases (`GET /v0/management/cursor-model-aliases`).
func (h *Handler) GetCursorModelAliases(c *gin.Context) {
	h.mu.Lock()
	aliases := append([]config.CursorModelAlias{}, h.cfg.CursorModelAliases...)
	h.mu.Unlock()
	c.JSON(200, gin.H{cursorModelAliasesField: aliases})
}

// PutCursorModelAliases replaces the Cursor effort aliases (`PUT /v0/management/cursor-model-aliases`).
func (h *Handler) PutCursorModelAliases(c *gin.Context) {
	var body map[string][]config.CursorModelAlias
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	aliases, ok := body[cursorModelAliasesField]
	if !ok {
		c.JSON(400, gin.H{"error": "missing " + cursorModelAliasesField})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.CursorModelAliases = aliases
	h.cfg.SanitizeModelExposure()
	h.persistLocked(c)
}

// GetAPIKeyPolicies returns the per-key model access policies (`GET /v0/management/api-key-policies`).
// Keys are the client keys /api-keys already returns to the same management caller.
func (h *Handler) GetAPIKeyPolicies(c *gin.Context) {
	h.mu.Lock()
	policies := append([]config.APIKeyPolicy{}, h.cfg.APIKeyPolicies...)
	h.mu.Unlock()
	c.JSON(200, gin.H{keyPoliciesField: policies})
}

// PutAPIKeyPolicies replaces all per-key policies (`PUT /v0/management/api-key-policies`).
func (h *Handler) PutAPIKeyPolicies(c *gin.Context) {
	var body map[string][]config.APIKeyPolicy
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	policies, ok := body[keyPoliciesField]
	if !ok {
		c.JSON(400, gin.H{"error": "missing " + keyPoliciesField})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.policiesTargetKnownKeysLocked(c, policies) {
		return
	}
	h.cfg.APIKeyPolicies = policies
	h.cfg.SanitizeModelExposure()
	h.persistLocked(c)
}

// PatchAPIKeyPolicy creates or replaces the policy of one key (`PATCH /v0/management/api-key-policies`).
// The body is one policy object, or {"key-policies": [policy]}.
func (h *Handler) PatchAPIKeyPolicy(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var policy config.APIKeyPolicy
	var wrapped map[string][]config.APIKeyPolicy
	if errWrapped := json.Unmarshal(data, &wrapped); errWrapped == nil && len(wrapped[keyPoliciesField]) == 1 {
		policy = wrapped[keyPoliciesField][0]
	} else if errSingle := json.Unmarshal(data, &policy); errSingle != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	policy.Key = strings.TrimSpace(policy.Key)
	if policy.Key == "" {
		c.JSON(400, gin.H{"error": "key is required"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.policiesTargetKnownKeysLocked(c, []config.APIKeyPolicy{policy}) {
		return
	}
	kept := make([]config.APIKeyPolicy, 0, len(h.cfg.APIKeyPolicies)+1)
	for _, existing := range h.cfg.APIKeyPolicies {
		if existing.Key != policy.Key {
			kept = append(kept, existing)
		}
	}
	h.cfg.APIKeyPolicies = append(kept, policy)
	h.cfg.SanitizeModelExposure()
	h.persistLocked(c)
}

// policiesTargetKnownKeysLocked rejects policies for keys that are not client keys, so a policy
// cannot be left behind for a key that does not exist. The key is not echoed in the error.
func (h *Handler) policiesTargetKnownKeysLocked(c *gin.Context, policies []config.APIKeyPolicy) bool {
	known := make(map[string]struct{}, len(h.cfg.APIKeys))
	for _, k := range h.cfg.APIKeys {
		known[strings.TrimSpace(k)] = struct{}{}
	}
	for i, p := range policies {
		if _, ok := known[strings.TrimSpace(p.Key)]; !ok {
			c.JSON(400, gin.H{"error": "policy " + strconv.Itoa(i) + " names a key that is not in api-keys"})
			return false
		}
	}
	return true
}

// catalogModel is one registered model as the management UI shows it.
type catalogModel struct {
	Provider     string `json:"provider"`
	Prefix       string `json:"prefix"`
	ID           string `json:"id"`
	ExposedID    string `json:"exposed_id"`
	DisplayName  string `json:"display_name"`
	OwnedBy      string `json:"owned_by"`
	Exposed      bool   `json:"exposed"`
	Reason       string `json:"reason"`
	CursorFamily string `json:"cursor_family,omitempty"`
	EffortLevel  string `json:"effort_level,omitempty"`
}

// GetModelExposureCatalog lists every registered model with its exposure under the saved
// configuration, plus combos and Cursor aliases (`GET /v0/management/model-exposure/catalog`).
func (h *Handler) GetModelExposureCatalog(c *gin.Context) {
	h.mu.Lock()
	exposure := h.cfg.ModelExposure
	combos := append([]config.ComboConfig{}, h.cfg.Combos...)
	aliases := append([]config.CursorModelAlias{}, h.cfg.CursorModelAliases...)
	h.mu.Unlock()

	registered := registry.GetGlobalRegistry().RegisteredProviderModels()
	models := make([]catalogModel, 0, len(registered))
	for _, pm := range registered {
		key := modelexposure.ProviderKey(pm.Provider)
		id := pm.Info.ID
		exposed, reason := modelexposure.Decide(&exposure, key, id)
		entry := catalogModel{
			Provider:    key,
			Prefix:      modelexposure.Settings(&exposure, key).Prefix,
			ID:          id,
			ExposedID:   modelexposure.ExposedID(&exposure, key, id),
			DisplayName: pm.Info.DisplayName,
			OwnedBy:     pm.Info.OwnedBy,
			Exposed:     exposed,
			Reason:      reason,
		}
		if key == "cursor" {
			family, level, _ := cursor.ParseVariantSuffix(modelexposure.ShortID(key, id))
			entry.CursorFamily = cursor.ClientModelID(family)
			entry.EffortLevel = level
		}
		models = append(models, entry)
	}
	c.JSON(200, gin.H{
		"models":                models,
		"combos":                combos,
		cursorModelAliasesField: aliases,
	})
}
