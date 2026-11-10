package helps

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	opencodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/opencode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// OpenAICompatHasExplicitWireAPI reports whether compat or attrWireAPI explicitly specifies
// a wire API ("chat" or "responses"), allowing callers to apply provider-specific defaults
// only when no explicit wire-api configuration is present.
func OpenAICompatHasExplicitWireAPI(compat *config.OpenAICompatibility, attrWireAPI, upstreamModel, requestedModel string) bool {
	if compat != nil {
		for _, model := range []string{upstreamModel, requestedModel} {
			if _, matched := openAICompatibilityModelWireAPI(compat.Models, model); matched {
				return true
			}
		}
		if strings.TrimSpace(compat.WireAPI) != "" {
			if _, ok := config.NormalizeOpenAICompatWireAPI(compat.WireAPI); ok {
				return true
			}
		}
	}
	if strings.TrimSpace(attrWireAPI) != "" {
		if _, ok := config.NormalizeOpenAICompatWireAPI(attrWireAPI); ok {
			return true
		}
	}
	return false
}

// IsOpencodeUpstream reports whether an OpenAI-compatible request targets the
// OpenCode Go gateway: either the built-in "opencode" / "opencode-go" / "ocg"
// provider, a compat entry named "opencode" / "opencode-go" / "ocg", or a base
// URL pointing at opencode.ai/zen/go.
func IsOpencodeUpstream(provider string, auth *cliproxyauth.Auth, compat *config.OpenAICompatibility, baseURL string) bool {
	candidates := []string{provider}
	if trimmed := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provider)), "openai-compatible-"); trimmed != "" {
		candidates = append(candidates, trimmed)
	}
	if auth != nil {
		candidates = append(candidates, auth.Provider)
		if auth.Attributes != nil {
			candidates = append(candidates, auth.Attributes["compat_name"], auth.Attributes["provider_key"])
		}
	}
	if compat != nil {
		candidates = append(candidates, compat.Name)
	}
	for _, candidate := range candidates {
		if opencodeauth.IsOpencodeProvider(candidate) {
			return true
		}
	}
	lowerURL := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.Contains(lowerURL, "opencode.ai/zen/go")
}

// ShouldUseOpencodeResponsesWire reports whether an OpenCode Go request should use
// the Responses API (/zen/go/v1/responses) instead of /chat/completions.
func ShouldUseOpencodeResponsesWire(from sdktranslator.Format, upstreamModel, requestedModel string) bool {
	if registry.IsOpencodeResponsesOnlyModel(upstreamModel) || registry.IsOpencodeResponsesOnlyModel(requestedModel) {
		return true
	}
	if from == sdktranslator.FormatOpenAIResponse {
		return registry.OpencodeSupportsResponses(upstreamModel) || registry.OpencodeSupportsResponses(requestedModel)
	}
	return false
}

// OpenAICompatUsesResponsesWire reports whether an OpenAI-compatible request must be
// sent to the upstream Responses API ({base-url}/responses) instead of Chat Completions.
//
// Resolution order: the matching model entry's wire-api (upstream name first, then
// alias), the provider-level wire-api, and finally attrWireAPI (the auth attribute
// used when no provider configuration is resolvable, e.g. Home-managed credentials).
func OpenAICompatUsesResponsesWire(compat *config.OpenAICompatibility, attrWireAPI, upstreamModel, requestedModel string) bool {
	if compat != nil {
		for _, model := range []string{upstreamModel, requestedModel} {
			if wire, matched := openAICompatibilityModelWireAPI(compat.Models, model); matched {
				return wire == config.OpenAICompatWireAPIResponses
			}
		}
		if strings.TrimSpace(compat.WireAPI) != "" {
			return compat.UsesResponsesWireAPI()
		}
	}
	normalized, _ := config.NormalizeOpenAICompatWireAPI(attrWireAPI)
	return normalized == config.OpenAICompatWireAPIResponses
}

// openAICompatibilityModelWireAPI returns the normalized per-model wire-api override.
// matched is false when no model entry matches or the matching entry inherits the provider value.
func openAICompatibilityModelWireAPI(models []config.OpenAICompatibilityModel, model string) (string, bool) {
	model = normalizeOpenAICompatibilityModelName(model)
	if model == "" {
		return "", false
	}
	resolve := func(raw string) (string, bool) {
		if strings.TrimSpace(raw) == "" {
			return "", false
		}
		normalized, ok := config.NormalizeOpenAICompatWireAPI(raw)
		if !ok {
			return "", false
		}
		if normalized == "" {
			normalized = config.OpenAICompatWireAPIChat
		}
		return normalized, true
	}
	for i := range models {
		if strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Name)) {
			return resolve(models[i].WireAPI)
		}
	}
	for i := range models {
		if strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Alias)) {
			return resolve(models[i].WireAPI)
		}
	}
	return "", false
}

// OpenAICompatResponsesTarget selects the translator target used for a Responses-wire
// OpenAI-compatible upstream. Responses clients are forwarded natively (identity), while
// other client protocols reuse the Responses-shaped "codex" translators.
func OpenAICompatResponsesTarget(from sdktranslator.Format) sdktranslator.Format {
	if from == sdktranslator.FormatOpenAIResponse {
		return sdktranslator.FormatOpenAIResponse
	}
	return sdktranslator.FormatCodex
}

// PrepareOpenAICompatResponsesRequestDoc adapts a translated Responses request for a
// generic third-party Responses upstream. It must run before thinking and payload rules.
//
// The Responses-shaped translators always emit a default reasoning block (and request
// encrypted reasoning content) because Codex models are reasoning models. Generic
// upstreams may host non-reasoning models that reject the reasoning parameter, so when
// the client expressed no reasoning intent (body or model suffix) those defaults are removed.
func PrepareOpenAICompatResponsesRequestDoc(doc *ojson.Doc, from, to sdktranslator.Format, sourcePayload []byte, model string, stream bool) {
	if doc == nil {
		return
	}
	if stream {
		SetBoolIfDifferentDoc(doc, "stream", true)
	} else {
		_ = doc.Delete("stream")
	}
	if to == sdktranslator.FormatOpenAIResponse {
		return
	}
	if thinking.ExtractReasoningEffort(sourcePayload, from.String(), model) != "" {
		return
	}
	_ = doc.Delete("reasoning")
	include := doc.Get("include")
	if !include.IsArray() {
		return
	}
	kept := make([]string, 0, len(include.Array()))
	removed := false
	for _, item := range include.Array() {
		if item.String() == "reasoning.encrypted_content" {
			removed = true
			continue
		}
		kept = append(kept, item.String())
	}
	if !removed {
		return
	}
	if len(kept) == 0 {
		_ = doc.Delete("include")
		return
	}
	_ = doc.Set("include", kept)
}

// IsResponsesTerminalEventType reports whether a Responses stream event type ends the stream successfully.
func IsResponsesTerminalEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.incomplete", "response.done":
		return true
	default:
		return false
	}
}

// OpenAICompatResponsesNonStreamPayload normalizes a non-streaming Responses upstream body.
// It returns the terminal event ({"type":"response.completed","response":{...}}) consumed by
// the Responses-shaped translators and usage parsers, plus the bare response object.
// Upstreams that answer with SSE despite stream=false are tolerated by extracting the
// terminal event. A failed response or a body without a terminal response is an error.
func OpenAICompatResponsesNonStreamPayload(body []byte) (event []byte, response []byte, err error) {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = extractResponsesTerminalSSEData(trimmed)
		if len(trimmed) == 0 {
			return nil, nil, fmt.Errorf("upstream responses stream ended without a terminal response event")
		}
	}
	if len(trimmed) == 0 || !ojson.ValidBytes(trimmed) {
		return nil, nil, fmt.Errorf("upstream returned an invalid responses payload")
	}
	eventType := ojson.GetBytesNoCopy(trimmed, "type").String()
	if strings.HasPrefix(eventType, "response.") {
		responseNode := ojson.GetBytesNoCopy(trimmed, "response")
		if !responseNode.IsObject() {
			return nil, nil, fmt.Errorf("upstream responses event %s carries no response object", eventType)
		}
		if eventType == "response.failed" || strings.EqualFold(responseNode.Get("status").String(), "failed") {
			return nil, nil, fmt.Errorf("upstream response failed: %s", responseNode.Get("error").Raw)
		}
		if eventType == "response.done" {
			eventType = "response.completed"
			if strings.EqualFold(responseNode.Get("status").String(), "incomplete") {
				eventType = "response.incomplete"
			}
			doc := ojson.NewDocString(`{}`)
			_ = doc.Set("type", eventType)
			_ = doc.SetRaw("response", responseNode.Raw)
			return doc.Bytes(), []byte(responseNode.Raw), nil
		}
		if !IsResponsesTerminalEventType(eventType) {
			return nil, nil, fmt.Errorf("upstream responses payload is not terminal: %s", eventType)
		}
		return bytes.Clone(trimmed), []byte(responseNode.Raw), nil
	}
	status := ojson.GetBytesNoCopy(trimmed, "status").String()
	if strings.EqualFold(status, "failed") {
		return nil, nil, fmt.Errorf("upstream response failed: %s", ojson.GetBytesNoCopy(trimmed, "error").Raw)
	}
	eventType = "response.completed"
	if strings.EqualFold(status, "incomplete") {
		eventType = "response.incomplete"
	}
	doc := ojson.NewDocString(`{}`)
	_ = doc.Set("type", eventType)
	_ = doc.SetRawBytes("response", trimmed)
	return doc.Bytes(), bytes.Clone(trimmed), nil
}

// extractResponsesTerminalSSEData returns the data payload of the last terminal or failed
// Responses event found in an SSE body.
func extractResponsesTerminalSSEData(body []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(nil, 52_428_800)
	var terminal []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if !ojson.ValidBytes(data) {
			continue
		}
		eventType := ojson.GetBytesNoCopy(data, "type").String()
		if IsResponsesTerminalEventType(eventType) || eventType == "response.failed" {
			terminal = bytes.Clone(data)
		}
	}
	return terminal
}
