package config

import "strings"

const (
	// OpenAICompatWireAPIChat sends requests to the upstream {base-url}/chat/completions endpoint.
	// It is the default when wire-api is omitted.
	OpenAICompatWireAPIChat = "chat"
	// OpenAICompatWireAPIResponses sends requests to the upstream {base-url}/responses endpoint
	// using the OpenAI Responses API wire format.
	OpenAICompatWireAPIResponses = "responses"
)

// NormalizeOpenAICompatWireAPI canonicalizes a wire-api value.
// It returns "" for the default Chat Completions wire, OpenAICompatWireAPIResponses for
// the Responses wire, and ok=false when the value is not recognized.
func NormalizeOpenAICompatWireAPI(value string) (normalized string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "chat", "chat-completions", "chat_completions", "chat/completions", "openai":
		return "", true
	case "responses", "response", "openai-response", "openai-responses":
		return OpenAICompatWireAPIResponses, true
	default:
		return "", false
	}
}

// UsesResponsesWireAPI reports whether the provider-level wire-api selects the Responses API.
func (c *OpenAICompatibility) UsesResponsesWireAPI() bool {
	if c == nil {
		return false
	}
	normalized, _ := NormalizeOpenAICompatWireAPI(c.WireAPI)
	return normalized == OpenAICompatWireAPIResponses
}
