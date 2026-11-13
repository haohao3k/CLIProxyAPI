package modelexposure

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The management UI reimplements these rules to preview exposure; this pins the contract.
func TestMatchAndDecideRules(t *testing.T) {
	for _, tc := range []struct {
		pattern, value string
		want           bool
	}{
		{"gpt-5.6*", "GPT-5.6-sol", true},
		{"*-fast", "gpt-5.6-fast", true},
		{"claude-*-5", "claude-opus-5", true},
		{"claude", "claude-opus", false}, // full match, not substring
		{"a*a", "a", false},
		{"", "", false},
	} {
		if got := Match(tc.pattern, tc.value); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}

	exposure := &config.ModelExposureConfig{Enabled: true, Providers: map[string]config.ModelExposureProvider{
		"codex":  {Prefix: "codex", Mode: "all", Deny: []string{"*-mini"}},
		"cursor": {Prefix: "cu", Mode: "allowlist", Allow: []string{"claude-*"}, Deny: []string{"*-fast"}},
		"zai":    {Prefix: "zai", Mode: "none", Allow: []string{"*"}},
	}}
	for _, tc := range []struct {
		key, id, reason string
		exposed         bool
	}{
		{"codex", "gpt-6", ReasonMode, true},
		{"codex", "gpt-6-mini", ReasonDeny, false}, // deny applies in mode all
		{"cursor", "cursor-claude-opus-5", ReasonAllow, true},
		{"cursor", "cursor-claude-opus-5-fast", ReasonDeny, false},
		{"cursor", "cursor-gpt-5", ReasonAllow, false},
		{"zai", "glm-5", ReasonMode, false}, // none hides everything
		{"opencode", "anything", ReasonMode, true},
	} {
		exposed, reason := Decide(exposure, tc.key, tc.id)
		if exposed != tc.exposed || reason != tc.reason {
			t.Errorf("Decide(%s, %s) = %v/%s, want %v/%s", tc.key, tc.id, exposed, reason, tc.exposed, tc.reason)
		}
	}
	if got := ExposedID(exposure, "cursor", "cursor-claude-opus-5"); got != "cu/claude-opus-5" {
		t.Errorf("ExposedID = %q", got)
	}
	if got := ExposedID(exposure, ProviderKey("openai-compatible-zcode-proxy"), "glm-5"); got != "zcode-proxy/glm-5" {
		t.Errorf("ExposedID for compat provider = %q", got)
	}
}
