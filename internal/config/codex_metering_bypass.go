package config

import (
	"os"
	"strconv"
	"strings"
)

func envBoolOverride(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func envIntOverride(name string) (int, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// MeteringBypassEnabled reports whether Codex stream metering bypass is enabled.
func (c *CodexConfig) MeteringBypassEnabled() bool {
	if v, ok := envBoolOverride("CODEX_STREAM_METERING_BYPASS"); ok {
		return v
	}
	return c != nil && c.MeteringBypass != nil && *c.MeteringBypass
}

// AutocutFinalizeEnabled reports whether __finalize_9f3 forced-finalize mode is active.
// Defaults to true whenever MeteringBypass is enabled unless explicitly set to false.
func (c *CodexConfig) AutocutFinalizeEnabled() bool {
	if v, ok := envBoolOverride("CODEX_AUTOCUT_FINALIZE"); ok {
		return v
	}
	if c != nil && c.AutocutFinalize != nil {
		return *c.AutocutFinalize
	}
	return true
}

// EffectiveAutocutReinforcement returns "always" (default), "adaptive", or "off".
func (c *CodexConfig) EffectiveAutocutReinforcement() string {
	raw := strings.TrimSpace(os.Getenv("CODEX_AUTOCUT_REINFORCEMENT"))
	if raw == "" && c != nil {
		raw = strings.TrimSpace(c.AutocutReinforcement)
	}
	switch strings.ToLower(raw) {
	case "adaptive":
		return "adaptive"
	case "off", "false", "none":
		return "off"
	default:
		return "always"
	}
}

// EffectiveAutocutMaxToolChars returns the prompt tool-argument ceiling (default 8000 — Iron Rule #2).
func (c *CodexConfig) EffectiveAutocutMaxToolChars() int {
	if v, ok := envIntOverride("CODEX_AUTOCUT_MAX_TOOL_CHARS"); ok && v >= 2000 {
		return v
	}
	if c != nil && c.AutocutMaxToolChars >= 2000 {
		return c.AutocutMaxToolChars
	}
	return 8000
}

// EffectiveAutocutHardArgCap returns the server-side hard argument cap (default 8000 — Iron Rule #2).
func (c *CodexConfig) EffectiveAutocutHardArgCap() int {
	if v, ok := envIntOverride("CODEX_AUTOCUT_HARD_ARG_CAP"); ok && v >= 0 {
		return v
	}
	if c != nil && c.AutocutHardArgCap > 0 {
		return c.AutocutHardArgCap
	}
	return 8000
}

// EffectiveMaxConcurrentRequests returns the max concurrent active Codex streams (default 2).
func (c *CodexConfig) EffectiveMaxConcurrentRequests() int {
	if v, ok := envIntOverride("CODEX_MAX_CONCURRENT_REQUESTS"); ok && v > 0 {
		return v
	}
	if c != nil && c.MaxConcurrentRequests > 0 {
		return c.MaxConcurrentRequests
	}
	return 2
}

// EffectiveQuotaCircuitBreakerMinRemaining returns the 5h remaining-% threshold (default 30, 0 disables).
func (c *CodexConfig) EffectiveQuotaCircuitBreakerMinRemaining() int {
	if v, ok := envIntOverride("CODEX_QUOTA_CIRCUIT_BREAKER_MIN_REMAINING"); ok {
		if v <= 0 {
			return 0
		}
		return v
	}
	if c != nil && c.QuotaCircuitBreakerMinRemaining != nil {
		if *c.QuotaCircuitBreakerMinRemaining <= 0 {
			return 0
		}
		return *c.QuotaCircuitBreakerMinRemaining
	}
	return 30
}

// AutocompactActive reports whether server-side context compaction is active when MeteringBypass is on.
func (c *CodexConfig) AutocompactActive() bool {
	if v, ok := envBoolOverride("CODEX_AUTOCOMPACT_ENABLED"); ok {
		return v
	}
	if c != nil && c.AutocompactEnabled != nil {
		return *c.AutocompactEnabled
	}
	return true
}

// EffectiveAutocompactThreshold returns the token threshold triggering compaction (default 150000).
func (c *CodexConfig) EffectiveAutocompactThreshold() int {
	if v, ok := envIntOverride("CODEX_AUTOCOMPACT_THRESHOLD"); ok && v > 0 {
		return v
	}
	if c != nil && c.AutocompactThreshold > 0 {
		return c.AutocompactThreshold
	}
	return 150000
}

// EffectiveAutocompactKeepTokens returns the verbatim tail token budget (default 40000).
func (c *CodexConfig) EffectiveAutocompactKeepTokens() int {
	if v, ok := envIntOverride("CODEX_AUTOCOMPACT_KEEP_TOKENS"); ok && v > 0 {
		return v
	}
	if c != nil && c.AutocompactKeepTokens > 0 {
		return c.AutocompactKeepTokens
	}
	return 40000
}

// EffectiveAutocompactHardCapTokens returns the last-line window-trim token cap (default 250000, 0 disables).
func (c *CodexConfig) EffectiveAutocompactHardCapTokens() int {
	if v, ok := envIntOverride("CODEX_AUTOCOMPACT_HARD_CAP_TOKENS"); ok {
		if v <= 0 {
			return 0
		}
		return v
	}
	if c != nil && c.AutocompactHardCapTokens != nil {
		if *c.AutocompactHardCapTokens <= 0 {
			return 0
		}
		return *c.AutocompactHardCapTokens
	}
	return 250000
}

// EffectiveAutocompactExternalModel returns the primary cheap summarizer model/combo (default "deepseek-v4.1-flash").
func (c *CodexConfig) EffectiveAutocompactExternalModel() string {
	if raw := strings.TrimSpace(os.Getenv("CODEX_AUTOCOMPACT_EXTERNAL_MODEL")); raw != "" {
		return raw
	}
	if c != nil && strings.TrimSpace(c.AutocompactExternalModel) != "" {
		return strings.TrimSpace(c.AutocompactExternalModel)
	}
	return "deepseek-v4.1-flash"
}

// EffectiveAutocompactExternalModels returns fallback summarizer models/combos (default ["glm-5.3-flash"]).
func (c *CodexConfig) EffectiveAutocompactExternalModels() []string {
	raw := strings.TrimSpace(os.Getenv("CODEX_AUTOCOMPACT_EXTERNAL_MODELS"))
	if raw == "" && c != nil {
		raw = strings.TrimSpace(c.AutocompactExternalModels)
	}
	if raw == "" {
		raw = "glm-5.3-flash"
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// CompactHijackEnabled reports whether client /compact hijack is active (default true when MeteringBypass is on).
func (c *CodexConfig) CompactHijackEnabled() bool {
	if !c.MeteringBypassEnabled() {
		return false
	}
	if v, ok := envBoolOverride("CODEX_COMPACT_HIJACK"); ok {
		return v
	}
	if c != nil && c.CompactHijack != nil {
		return *c.CompactHijack
	}
	return true
}

// EffectiveAllowHighEffort reports whether high/xhigh/max/ultra reasoning effort is allowed without medium clamp.
func (c *CodexConfig) EffectiveAllowHighEffort() bool {
	if v, ok := envBoolOverride("CODEX_ALLOW_HIGH"); ok {
		return v
	}
	return c != nil && c.AllowHighEffort
}

// EffectiveAllowSolModel reports whether -sol models are allowed without clamping to gpt-6-astra.
func (c *CodexConfig) EffectiveAllowSolModel() bool {
	if v, ok := envBoolOverride("CODEX_ALLOW_SOL"); ok {
		return v
	}
	return c != nil && c.AllowSolModel
}

// EffectiveAllowChatCompletions reports whether non-Responses formats (/v1/chat/completions, /v1/messages)
// are allowed when MeteringBypass is enabled. Defaults to false (Responses API only, matching 9router :9gl-med).
func (c *CodexConfig) EffectiveAllowChatCompletions() bool {
	if v, ok := envBoolOverride("CODEX_ALLOW_CHAT_COMPLETIONS"); ok {
		return v
	}
	return c != nil && c.AllowChatCompletions
}

// CodexCompactHijackEnabled reports whether client /compact hijack is active on SDKConfig.
func (c *SDKConfig) CodexCompactHijackEnabled() bool {
	if v, ok := envBoolOverride("CODEX_COMPACT_HIJACK"); ok {
		return v
	}
	if c != nil && c.CodexCompactHijack != nil {
		return *c.CodexCompactHijack
	}
	return false
}

// EffectiveCodexCompactHijackModel returns the target model/combo for hijacked client /compact turns.
func (c *SDKConfig) EffectiveCodexCompactHijackModel() string {
	if raw := strings.TrimSpace(os.Getenv("CODEX_COMPACT_HIJACK_MODEL")); raw != "" {
		return raw
	}
	if raw := strings.TrimSpace(os.Getenv("CODEX_AUTOCOMPACT_EXTERNAL_MODEL")); raw != "" {
		return raw
	}
	if c != nil && strings.TrimSpace(c.CodexCompactHijackModel) != "" {
		return strings.TrimSpace(c.CodexCompactHijackModel)
	}
	return "deepseek-v4.1-flash"
}
