package helps

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestIsZAIUpstream(t *testing.T) {
	cases := []struct {
		provider, baseURL string
		want              bool
	}{
		{"zai", "", true},
		{"ZAI", "https://example.com", true},
		{"openai-compatibility", "https://api.z.ai/api/coding/paas/v4", true},
		{"glm", "https://open.bigmodel.cn/api/coding/paas/v4", true},
		{"openrouter", "https://openrouter.ai/api/v1", false},
		{"evil", "https://api.z.ai.example.com/v1", false},
	}
	for _, tc := range cases {
		if got := IsZAIUpstream(tc.provider, tc.baseURL); got != tc.want {
			t.Errorf("IsZAIUpstream(%q, %q) = %v, want %v", tc.provider, tc.baseURL, got, tc.want)
		}
	}
}

func TestNormalizeZAIThinkingDoc(t *testing.T) {
	cases := []struct {
		name, in, want string
		changed        bool
	}{
		{"enable", `{"model":"glm-5.2","reasoning_effort":"high"}`, `{"model":"glm-5.2","thinking":{"type":"enabled"}}`, true},
		{"disable", `{"model":"glm-5.2","reasoning_effort":"none"}`, `{"model":"glm-5.2","thinking":{"type":"disabled"}}`, true},
		{"merge existing object", `{"reasoning_effort":"low","thinking":{"type":"disabled","clear_thinking":false}}`, `{"thinking":{"type":"enabled","clear_thinking":false}}`, true},
		{"untouched", `{"model":"glm-5.2","thinking":{"type":"enabled"}}`, `{"model":"glm-5.2","thinking":{"type":"enabled"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(tc.in)
			changed := NormalizeZAIThinkingDoc(doc)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			got := ojson.NewDocString(doc.String())
			want := ojson.NewDocString(tc.want)
			if got.Get("thinking.type").String() != want.Get("thinking.type").String() ||
				got.Get("reasoning_effort").Exists() ||
				got.Get("thinking.clear_thinking").Raw != want.Get("thinking.clear_thinking").Raw {
				t.Fatalf("got %s, want %s", doc.String(), tc.want)
			}
		})
	}
}
