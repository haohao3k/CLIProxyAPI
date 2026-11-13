package config

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// Model exposure modes for a provider.
const (
	ModelExposureModeAll       = "all"
	ModelExposureModeAllowlist = "allowlist"
	ModelExposureModeNone      = "none"
)

// ModelExposureConfig controls which registered models clients may list and call directly,
// and the "<prefix>/<id>" names they use for them. Disabled keeps the unfiltered behaviour.
type ModelExposureConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ListPrefixedOnly lists combos and "<prefix>/<id>" names only, without bare ids.
	ListPrefixedOnly bool `yaml:"list-prefixed-only" json:"list-prefixed-only"`
	// AcceptBareIDs lets requests name an exposed model by its bare id. Omitted means true.
	AcceptBareIDs bool `yaml:"accept-bare-ids" json:"accept-bare-ids"`
	// Providers is keyed by provider key (codex, cursor, opencode, mistral, zai, ...) or by an
	// openai-compatibility entry name. A provider without an entry uses mode "all" and its key as prefix.
	Providers map[string]ModelExposureProvider `yaml:"providers,omitempty" json:"providers"`
}

// IsZero reports the default section (disabled, bare ids accepted, no providers), which the
// YAML encoder then omits like an absent section.
func (e ModelExposureConfig) IsZero() bool {
	return !e.Enabled && !e.ListPrefixedOnly && e.AcceptBareIDs && len(e.Providers) == 0
}

type modelExposureFields ModelExposureConfig

// UnmarshalYAML decodes the section with accept-bare-ids defaulting to true, so a hand-written
// section that omits it keeps bare-id clients working.
func (e *ModelExposureConfig) UnmarshalYAML(node *yaml.Node) error {
	fields := modelExposureFields{AcceptBareIDs: true}
	if err := node.Decode(&fields); err != nil {
		return err
	}
	*e = ModelExposureConfig(fields)
	return nil
}

// UnmarshalJSON decodes the section with accept-bare-ids defaulting to true.
func (e *ModelExposureConfig) UnmarshalJSON(data []byte) error {
	fields := modelExposureFields{AcceptBareIDs: true}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*e = ModelExposureConfig(fields)
	return nil
}

// ModelExposureProvider is the exposure rule for one provider.
type ModelExposureProvider struct {
	Prefix string   `yaml:"prefix,omitempty" json:"prefix"`
	Mode   string   `yaml:"mode,omitempty" json:"mode"`
	Allow  []string `yaml:"allow,omitempty" json:"allow"`
	Deny   []string `yaml:"deny,omitempty" json:"deny"`
}

// CursorModelAlias names a set of Cursor effort variants as one model.
type CursorModelAlias struct {
	Alias         string            `yaml:"alias" json:"alias"`
	DefaultEffort string            `yaml:"default-effort,omitempty" json:"default-effort"`
	Variants      map[string]string `yaml:"variants" json:"variants"`
}

// APIKeyPolicy restricts which exposed models and combos one client API key may call.
type APIKeyPolicy struct {
	Key        string   `yaml:"key" json:"key"`
	Restricted bool     `yaml:"restricted" json:"restricted"`
	Allow      []string `yaml:"allow,omitempty" json:"allow"`
}

// SanitizeModelExposure normalizes model exposure, Cursor aliases and API key policies.
func (c *SDKConfig) SanitizeModelExposure() {
	if c == nil {
		return
	}
	c.ModelExposure.sanitize()
	c.CursorSpendGuard.Sanitize()
	c.CursorModelAliases = sanitizeCursorModelAliases(c.CursorModelAliases)
	c.APIKeyPolicies = sanitizeAPIKeyPolicies(c.APIKeyPolicies)
}

func (e *ModelExposureConfig) sanitize() {
	if len(e.Providers) == 0 {
		e.Providers = nil
		return
	}
	out := make(map[string]ModelExposureProvider, len(e.Providers))
	for rawKey, p := range e.Providers {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		if key == "" {
			continue
		}
		p.Prefix = strings.Trim(strings.ToLower(strings.TrimSpace(p.Prefix)), "/")
		if strings.Contains(p.Prefix, "/") {
			p.Prefix = ""
		}
		if p.Prefix == "" {
			p.Prefix = key
		}
		switch strings.ToLower(strings.TrimSpace(p.Mode)) {
		case ModelExposureModeAllowlist:
			p.Mode = ModelExposureModeAllowlist
		case ModelExposureModeNone:
			p.Mode = ModelExposureModeNone
		default:
			p.Mode = ModelExposureModeAll
		}
		p.Allow = cleanPatternList(p.Allow)
		p.Deny = cleanPatternList(p.Deny)
		out[key] = p
	}
	if len(out) == 0 {
		out = nil
	}
	e.Providers = out
}

func cleanPatternList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		k := strings.ToLower(v)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeCursorModelAliases(in []CursorModelAlias) []CursorModelAlias {
	out := make([]CursorModelAlias, 0, len(in))
	index := make(map[string]int, len(in))
	for _, a := range in {
		a.Alias = strings.TrimSpace(a.Alias)
		a.DefaultEffort = strings.ToLower(strings.TrimSpace(a.DefaultEffort))
		if a.Alias == "" || strings.Contains(a.Alias, "/") {
			continue
		}
		variants := make(map[string]string, len(a.Variants))
		for level, target := range a.Variants {
			level = strings.ToLower(strings.TrimSpace(level))
			target = strings.TrimSpace(target)
			if level == "" || target == "" {
				continue
			}
			variants[level] = target
		}
		if len(variants) == 0 {
			continue
		}
		a.Variants = variants
		k := strings.ToLower(a.Alias)
		if i, ok := index[k]; ok {
			out[i] = a
			continue
		}
		index[k] = len(out)
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeAPIKeyPolicies merges duplicate entries for one key so that the strictest wins:
// any restricted entry makes the key restricted and only restricted entries contribute
// allow patterns.
func sanitizeAPIKeyPolicies(in []APIKeyPolicy) []APIKeyPolicy {
	out := make([]APIKeyPolicy, 0, len(in))
	index := make(map[string]int, len(in))
	for _, p := range in {
		p.Key = strings.TrimSpace(p.Key)
		if p.Key == "" {
			continue
		}
		p.Allow = cleanPatternList(p.Allow)
		i, dup := index[p.Key]
		if !dup {
			index[p.Key] = len(out)
			out = append(out, p)
			continue
		}
		prev := out[i]
		switch {
		case prev.Restricted && p.Restricted:
			prev.Allow = cleanPatternList(append(prev.Allow, p.Allow...))
		case p.Restricted:
			prev.Restricted = true
			prev.Allow = p.Allow
		}
		out[i] = prev
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SyncAPIKeyPolicies keeps policies attached to client keys after an api-keys edit:
// when exactly one key value changed in place the policy follows it, and policies of
// keys no longer present are dropped.
func (c *SDKConfig) SyncAPIKeyPolicies(before []string) {
	if c == nil || len(c.APIKeyPolicies) == 0 {
		return
	}
	if len(before) == len(c.APIKeys) {
		changed := -1
		for i := range before {
			if before[i] != c.APIKeys[i] {
				if changed >= 0 {
					changed = -1
					break
				}
				changed = i
			}
		}
		if changed >= 0 {
			oldKey := strings.TrimSpace(before[changed])
			newKey := strings.TrimSpace(c.APIKeys[changed])
			for i := range c.APIKeyPolicies {
				if c.APIKeyPolicies[i].Key == oldKey {
					c.APIKeyPolicies[i].Key = newKey
				}
			}
		}
	}
	present := make(map[string]struct{}, len(c.APIKeys))
	for _, k := range c.APIKeys {
		present[strings.TrimSpace(k)] = struct{}{}
	}
	kept := c.APIKeyPolicies[:0]
	for _, p := range c.APIKeyPolicies {
		if _, ok := present[p.Key]; ok {
			kept = append(kept, p)
		}
	}
	c.APIKeyPolicies = sanitizeAPIKeyPolicies(kept)
}
