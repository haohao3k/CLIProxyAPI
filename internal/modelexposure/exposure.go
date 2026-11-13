// Package modelexposure decides which registered models clients may list and call, the
// "<prefix>/<id>" names they use, and which of those names a client API key may call.
// The management UI reimplements Match and Decide; keep their rules in step with it.
package modelexposure

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Decision reasons reported to the management catalog.
const (
	ReasonAllow = "allow"
	ReasonDeny  = "deny"
	ReasonMode  = "mode"
	ReasonAlias = "alias"
)

// ComboPrefix marks a combo in an API key allow list.
const ComboPrefix = "combo:"

const openAICompatiblePrefix = "openai-compatible-"

// nativeIDPrefixes are prefixes a provider adds to its own model ids; exposed names drop them.
var nativeIDPrefixes = map[string]string{"cursor": "cursor-"}

// ProviderKey maps a registry provider identifier onto the key used in model-exposure.providers.
func ProviderKey(registryProvider string) string {
	key := strings.ToLower(strings.TrimSpace(registryProvider))
	key = strings.TrimPrefix(key, openAICompatiblePrefix)
	switch key {
	case "opencode-go", "ocg", "opencode_go":
		return "opencode"
	case "mistral-vibe":
		return "mistral"
	}
	return key
}

// Match reports whether pattern matches value in full, case-insensitively; '*' matches any run.
func Match(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" {
		return false
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	if len(value) < len(last) || !strings.HasSuffix(value, last) {
		return false
	}
	value = value[:len(value)-len(last)]
	for _, segment := range parts[1 : len(parts)-1] {
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}

func matchAny(patterns []string, value string) bool {
	for _, p := range patterns {
		if Match(p, value) {
			return true
		}
	}
	return false
}

// Settings returns the effective rule for a provider key.
func Settings(cfg *config.ModelExposureConfig, key string) config.ModelExposureProvider {
	key = strings.ToLower(strings.TrimSpace(key))
	if cfg != nil {
		if p, ok := cfg.Providers[key]; ok {
			if p.Prefix == "" {
				p.Prefix = key
			}
			if p.Mode == "" {
				p.Mode = config.ModelExposureModeAll
			}
			return p
		}
	}
	return config.ModelExposureProvider{Prefix: key, Mode: config.ModelExposureModeAll}
}

// ShortID drops the provider's native id prefix (cursor-claude-opus-5 -> claude-opus-5).
func ShortID(key, nativeID string) string {
	nativeID = strings.TrimSpace(nativeID)
	if prefix, ok := nativeIDPrefixes[key]; ok && len(nativeID) > len(prefix) && strings.EqualFold(nativeID[:len(prefix)], prefix) {
		return nativeID[len(prefix):]
	}
	return nativeID
}

// NativeCandidates returns the registry ids a short id may stand for under a provider.
func NativeCandidates(key, shortID string) []string {
	shortID = strings.TrimSpace(shortID)
	if prefix, ok := nativeIDPrefixes[key]; ok && !strings.HasPrefix(strings.ToLower(shortID), prefix) {
		return []string{prefix + shortID, shortID}
	}
	return []string{shortID}
}

// ExposedID is the name a provider's model is listed and matched under.
func ExposedID(cfg *config.ModelExposureConfig, key, nativeID string) string {
	return Settings(cfg, key).Prefix + "/" + ShortID(key, nativeID)
}

// Decide reports whether a provider's model is exposed and which rule decided it.
func Decide(cfg *config.ModelExposureConfig, key, nativeID string) (bool, string) {
	if cfg == nil || !cfg.Enabled {
		return true, ReasonMode
	}
	s := Settings(cfg, key)
	id := ShortID(key, nativeID)
	switch s.Mode {
	case config.ModelExposureModeNone:
		return false, ReasonMode
	case config.ModelExposureModeAllowlist:
		if !matchAny(s.Allow, id) {
			return false, ReasonAllow
		}
		if matchAny(s.Deny, id) {
			return false, ReasonDeny
		}
		return true, ReasonAllow
	default:
		if matchAny(s.Deny, id) {
			return false, ReasonDeny
		}
		return true, ReasonMode
	}
}

// FindAlias returns the Cursor alias with the given name, case-insensitively.
func FindAlias(aliases []config.CursorModelAlias, name string) *config.CursorModelAlias {
	name = strings.TrimSpace(name)
	for i := range aliases {
		if strings.EqualFold(aliases[i].Alias, name) {
			return &aliases[i]
		}
	}
	return nil
}

// Policy is the access rule of one client key.
type Policy struct {
	restricted bool
	allow      []string
}

// PolicyFor returns the policy of a client key. Keys without a policy are unrestricted.
func PolicyFor(policies []config.APIKeyPolicy, apiKey string) Policy {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return Policy{}
	}
	// Repeated entries for one key: any restricted entry restricts the key and only
	// restricted entries grant names, so the strictest reading wins.
	var out Policy
	for _, p := range policies {
		if strings.TrimSpace(p.Key) == apiKey && p.Restricted {
			out.restricted = true
			out.allow = append(out.allow, p.Allow...)
		}
	}
	return out
}

// Restricted reports whether the key is limited to its allow list.
func (p Policy) Restricted() bool { return p.restricted }

// AllowsName reports whether the key may call the exposed name ("<prefix>/<id>").
func (p Policy) AllowsName(exposedID string) bool {
	return !p.restricted || matchAny(p.allow, exposedID)
}

// AllowsCombo reports whether the key may call a combo, written "combo:<name>" or as the bare name.
func (p Policy) AllowsCombo(name string) bool {
	if !p.restricted {
		return true
	}
	name = strings.TrimSpace(name)
	for _, pattern := range p.allow {
		pattern = strings.TrimSpace(pattern)
		if len(pattern) > len(ComboPrefix) && strings.EqualFold(pattern[:len(ComboPrefix)], ComboPrefix) {
			if Match(pattern[len(ComboPrefix):], name) {
				return true
			}
			continue
		}
		if !strings.Contains(pattern, "/") && Match(pattern, name) {
			return true
		}
	}
	return false
}

// DecideWithAliases is Decide, except that a Cursor model whose exposed name a Cursor alias also
// takes is hidden: the alias owns that name and the model stays reachable only through it.
func DecideWithAliases(cfg *config.ModelExposureConfig, aliases []config.CursorModelAlias, key, nativeID string) (bool, string) {
	if key == "cursor" && FindAlias(aliases, ShortID(key, nativeID)) != nil {
		return false, ReasonAlias
	}
	return Decide(cfg, key, nativeID)
}
