package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
)

type codexMeteringBypassResponse struct {
	MeteringBypass                  bool   `json:"metering-bypass"`
	AutocutFinalize                 bool   `json:"autocut-finalize"`
	AutocutReinforcement            string `json:"autocut-reinforcement"`
	AutocutMaxToolChars             int    `json:"autocut-max-tool-chars"`
	AutocutHardArgCap               int    `json:"autocut-hard-arg-cap"`
	MaxConcurrentRequests           int    `json:"max-concurrent-requests"`
	QuotaCircuitBreakerMinRemaining int    `json:"quota-circuit-breaker-min-remaining"`
	AutocompactEnabled              bool   `json:"autocompact-enabled"`
	AutocompactThreshold            int    `json:"autocompact-threshold"`
	AutocompactKeepTokens           int    `json:"autocompact-keep-tokens"`
	AutocompactHardCapTokens        int    `json:"autocompact-hard-cap-tokens"`
	AutocompactExternalModel        string `json:"autocompact-external-model"`
	AutocompactExternalModels       string `json:"autocompact-external-models"`
	CompactHijack                   bool   `json:"compact-hijack"`
	AllowHighEffort                 bool   `json:"allow-high-effort"`
	AllowSolModel                   bool   `json:"allow-sol-model"`
}

type codexMeteringBypassUpdate struct {
	MeteringBypass                  *bool   `json:"metering-bypass"`
	AutocutFinalize                 *bool   `json:"autocut-finalize"`
	AutocutReinforcement            *string `json:"autocut-reinforcement"`
	AutocutMaxToolChars             *int    `json:"autocut-max-tool-chars"`
	AutocutHardArgCap               *int    `json:"autocut-hard-arg-cap"`
	MaxConcurrentRequests           *int    `json:"max-concurrent-requests"`
	QuotaCircuitBreakerMinRemaining *int    `json:"quota-circuit-breaker-min-remaining"`
	AutocompactEnabled              *bool   `json:"autocompact-enabled"`
	AutocompactThreshold            *int    `json:"autocompact-threshold"`
	AutocompactKeepTokens           *int    `json:"autocompact-keep-tokens"`
	AutocompactHardCapTokens        *int    `json:"autocompact-hard-cap-tokens"`
	AutocompactExternalModel        *string `json:"autocompact-external-model"`
	AutocompactExternalModels       *string `json:"autocompact-external-models"`
	CompactHijack                   *bool   `json:"compact-hijack"`
	AllowHighEffort                 *bool   `json:"allow-high-effort"`
	AllowSolModel                   *bool   `json:"allow-sol-model"`
}

// GetCodexMeteringBypass returns effective Codex metering-bypass settings (`GET /v0/management/codex-metering-bypass`).
func (h *Handler) GetCodexMeteringBypass(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, codexMeteringBypassResponse{})
		return
	}
	h.mu.Lock()
	cx := h.cfg.Codex
	h.mu.Unlock()
	c.JSON(http.StatusOK, codexMeteringBypassResponse{
		MeteringBypass:                  cx.MeteringBypassEnabled(),
		AutocutFinalize:                 cx.AutocutFinalizeEnabled(),
		AutocutReinforcement:            cx.EffectiveAutocutReinforcement(),
		AutocutMaxToolChars:             cx.EffectiveAutocutMaxToolChars(),
		AutocutHardArgCap:               cx.EffectiveAutocutHardArgCap(),
		MaxConcurrentRequests:           cx.EffectiveMaxConcurrentRequests(),
		QuotaCircuitBreakerMinRemaining: cx.EffectiveQuotaCircuitBreakerMinRemaining(),
		AutocompactEnabled:              cx.AutocompactActive(),
		AutocompactThreshold:            cx.EffectiveAutocompactThreshold(),
		AutocompactKeepTokens:           cx.EffectiveAutocompactKeepTokens(),
		AutocompactHardCapTokens:        cx.EffectiveAutocompactHardCapTokens(),
		AutocompactExternalModel:        cx.EffectiveAutocompactExternalModel(),
		AutocompactExternalModels:       strings.Join(cx.EffectiveAutocompactExternalModels(), ","),
		CompactHijack:                   cx.CompactHijack != nil && *cx.CompactHijack || cx.CompactHijack == nil,
		AllowHighEffort:                 cx.EffectiveAllowHighEffort(),
		AllowSolModel:                   cx.EffectiveAllowSolModel(),
	})
}

// PutCodexMeteringBypass updates Codex metering-bypass settings (`PUT/PATCH /v0/management/codex-metering-bypass`).
func (h *Handler) PutCodexMeteringBypass(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config not initialized"})
		return
	}
	var body codexMeteringBypassUpdate
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	cx := &h.cfg.Codex
	if body.MeteringBypass != nil {
		v := *body.MeteringBypass
		cx.MeteringBypass = &v
	}
	if body.AutocutFinalize != nil {
		v := *body.AutocutFinalize
		cx.AutocutFinalize = &v
	}
	if body.AutocutReinforcement != nil {
		val := strings.ToLower(strings.TrimSpace(*body.AutocutReinforcement))
		switch val {
		case "", "always", "adaptive", "off":
			cx.AutocutReinforcement = val
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid autocut-reinforcement (expected always, adaptive, or off)"})
			return
		}
	}
	if body.AutocutMaxToolChars != nil {
		if *body.AutocutMaxToolChars > 0 {
			cx.AutocutMaxToolChars = *body.AutocutMaxToolChars
		}
	}
	if body.AutocutHardArgCap != nil {
		if *body.AutocutHardArgCap >= 0 {
			cx.AutocutHardArgCap = *body.AutocutHardArgCap
		}
	}
	if body.MaxConcurrentRequests != nil {
		if *body.MaxConcurrentRequests > 0 {
			cx.MaxConcurrentRequests = *body.MaxConcurrentRequests
		}
	}
	if body.QuotaCircuitBreakerMinRemaining != nil {
		v := *body.QuotaCircuitBreakerMinRemaining
		if v < 0 {
			v = 0
		}
		cx.QuotaCircuitBreakerMinRemaining = &v
	}
	if body.AutocompactEnabled != nil {
		v := *body.AutocompactEnabled
		cx.AutocompactEnabled = &v
	}
	if body.AutocompactThreshold != nil && *body.AutocompactThreshold > 0 {
		cx.AutocompactThreshold = *body.AutocompactThreshold
	}
	if body.AutocompactKeepTokens != nil && *body.AutocompactKeepTokens > 0 {
		cx.AutocompactKeepTokens = *body.AutocompactKeepTokens
	}
	if body.AutocompactHardCapTokens != nil {
		v := *body.AutocompactHardCapTokens
		if v < 0 {
			v = 0
		}
		cx.AutocompactHardCapTokens = &v
	}
	if body.AutocompactExternalModel != nil {
		cx.AutocompactExternalModel = strings.TrimSpace(*body.AutocompactExternalModel)
	}
	if body.AutocompactExternalModels != nil {
		cx.AutocompactExternalModels = strings.TrimSpace(*body.AutocompactExternalModels)
	}
	if body.CompactHijack != nil {
		v := *body.CompactHijack
		cx.CompactHijack = &v
	}
	if body.AllowHighEffort != nil {
		cx.AllowHighEffort = *body.AllowHighEffort
	}
	if body.AllowSolModel != nil {
		cx.AllowSolModel = *body.AllowSolModel
	}

	h.persistLocked(c)
}

// GetCodexMeteringBypassTelemetry returns live + persisted forensic telemetry for Codex Autocut,
// Finalize, per-account 5h quota deltas, and leak-risk turns (`GET /v0/management/codex-metering-bypass/telemetry`).
func (h *Handler) GetCodexMeteringBypassTelemetry(c *gin.Context) {
	limit := 50
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	leaksOnly := false
	if rawLeaks := strings.ToLower(strings.TrimSpace(c.Query("leaks_only"))); rawLeaks == "1" || rawLeaks == "true" || rawLeaks == "yes" {
		leaksOnly = true
	}
	var logDir string
	if h != nil {
		logDir = h.logDirectory()
	}
	var cfgPtr = h.cfg
	snap := executor.SnapshotCodexAutocutTelemetry(cfgPtr, logDir, executor.CodexTelemetryQueryOptions{
		Limit:     limit,
		LeaksOnly: leaksOnly,
		SessionID: strings.TrimSpace(c.Query("session")),
		AuthQuery: strings.TrimSpace(c.Query("auth")),
	})
	c.JSON(http.StatusOK, snap)
}
