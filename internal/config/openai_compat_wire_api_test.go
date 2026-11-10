package config

import "testing"

func TestNormalizeOpenAICompatWireAPI(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"":                  {"", true},
		"chat":              {"", true},
		" Chat-Completions": {"", true},
		"responses":         {OpenAICompatWireAPIResponses, true},
		"OpenAI-Responses":  {OpenAICompatWireAPIResponses, true},
		"grpc":              {"", false},
	}
	for input, tc := range cases {
		got, ok := NormalizeOpenAICompatWireAPI(input)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("NormalizeOpenAICompatWireAPI(%q) = (%q, %t), want (%q, %t)", input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseConfigBytesOpenAICompatWireAPILegacyLayout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
openai-compatibility:
  - name: "responses-provider"
    base-url: "https://api.example.com/v1"
    wire-api: "Responses"
    api-key-entries:
      - api-key: "k1"
    models:
      - name: "chat-only"
        alias: "chat-only"
        wire-api: "chat"
      - name: "inherit"
        alias: "inherit"
  - name: "chat-provider"
    base-url: "https://chat.example.com/v1"
    models:
      - name: "m"
        alias: "m"
        wire-api: "bogus"
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 2 {
		t.Fatalf("openai-compatibility count = %d, want 2", len(cfg.OpenAICompatibility))
	}
	responsesProvider := cfg.OpenAICompatibility[0]
	if responsesProvider.WireAPI != OpenAICompatWireAPIResponses || !responsesProvider.UsesResponsesWireAPI() {
		t.Fatalf("provider wire-api = %q, want responses", responsesProvider.WireAPI)
	}
	if responsesProvider.Models[0].WireAPI != "" || responsesProvider.Models[1].WireAPI != "" {
		t.Fatalf("model wire-api values = %q/%q, want normalized empty (chat)", responsesProvider.Models[0].WireAPI, responsesProvider.Models[1].WireAPI)
	}
	chatProvider := cfg.OpenAICompatibility[1]
	if chatProvider.WireAPI != "" || chatProvider.UsesResponsesWireAPI() {
		t.Fatalf("chat provider wire-api = %q, want default", chatProvider.WireAPI)
	}
	if chatProvider.Models[0].WireAPI != "" {
		t.Fatalf("invalid model wire-api must fall back to chat, got %q", chatProvider.Models[0].WireAPI)
	}
}

func TestParseConfigBytesOpenAICompatWireAPIV8Layout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
api-keys:
  openai-compatibility:
    - name: "openai-platform"
      base-url: "https://api.openai.com/v1"
      wire-api: "responses"
      keys:
        - api-key: "sk-test"
      models:
        - name: "gpt-5.1"
          alias: "gpt-5.1"
          wire-api: "responses"
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("openai-compatibility count = %d, want 1", len(cfg.OpenAICompatibility))
	}
	compat := cfg.OpenAICompatibility[0]
	if !compat.UsesResponsesWireAPI() {
		t.Fatalf("v8 provider wire-api = %q, want responses", compat.WireAPI)
	}
	if len(compat.APIKeyEntries) != 1 || compat.APIKeyEntries[0].APIKey != "sk-test" {
		t.Fatalf("v8 keys = %+v", compat.APIKeyEntries)
	}
	if compat.Models[0].WireAPI != OpenAICompatWireAPIResponses {
		t.Fatalf("v8 model wire-api = %q, want responses", compat.Models[0].WireAPI)
	}
}
