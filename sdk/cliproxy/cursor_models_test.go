package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCursorLiveModelsEnabledDefaultOn(t *testing.T) {
	s := &Service{}
	cases := []struct {
		name string
		auth *coreauth.Auth
		want bool
	}{
		{"nil", nil, false},
		{"default on", &coreauth.Auth{}, true},
		{"metadata bool false", &coreauth.Auth{Metadata: map[string]any{"live_models": false}}, false},
		{"metadata bool true", &coreauth.Auth{Metadata: map[string]any{"live_models": true}}, true},
		{"metadata string", &coreauth.Auth{Metadata: map[string]any{"live_models": "yes"}}, true},
		{"attribute wins", &coreauth.Auth{Attributes: map[string]string{"live_models": "false"}, Metadata: map[string]any{"live_models": true}}, false},
	}
	for _, tc := range cases {
		if got := s.cursorLiveModelsEnabled(tc.auth); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMergeCursorModels(t *testing.T) {
	static := []*registry.ModelInfo{{ID: "cursor-gpt-5", DisplayName: "Cursor GPT-5", ContextLength: 400000}}
	live := []*registry.ModelInfo{{ID: "cursor-gpt-5", DisplayName: "cursor-gpt-5"}, {ID: "cursor-new", DisplayName: "cursor-new"}}
	out := mergeCursorModels(live, static)
	if len(out) != 2 || out[0].ContextLength != 400000 || out[1].ID != "cursor-new" {
		t.Fatalf("unexpected merge: %+v", out)
	}
	if static[0].DisplayName != "Cursor GPT-5" {
		t.Fatalf("static catalog must not be mutated")
	}
	if got := mergeCursorModels(nil, static); len(got) != 1 {
		t.Fatalf("empty live must return static")
	}
}
