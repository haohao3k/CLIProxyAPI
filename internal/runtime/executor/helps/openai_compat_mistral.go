package helps

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

func isMistralBaseURL(baseURL string) bool {
	parsedURL, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), ".")
	return host == "mistral.ai" || strings.HasSuffix(host, ".mistral.ai")
}

// SanitizeMistralChatRequestDoc removes fields and normalizes message roles rejected
// by Mistral's Chat Completions schema (such as OpenAI Responses "developer" roles
// and mid-conversation "system" messages after assistant/tool turns). Run it after
// built-in request preparation and before payload rules so explicit operator
// configuration retains final precedence.
func SanitizeMistralChatRequestDoc(doc *ojson.Doc, baseURL string) {
	if doc == nil || !isMistralBaseURL(baseURL) {
		return
	}

	_ = doc.Delete("stream_options")
	// Mistral rejects the client_metadata field some clients attach.
	_ = doc.Delete("client_metadata")
	// Mistral's schema forbids extra fields (422 extra_forbidden) for these OpenAI
	// Chat Completions fields.
	for _, field := range []string{"store", "user", "logit_bias", "safety_identifier", "verbosity"} {
		_ = doc.Delete(field)
	}
	// OpenAI names with a Mistral equivalent: keep the value under Mistral's name.
	renameMistralFieldDoc(doc, "seed", "random_seed")
	renameMistralFieldDoc(doc, "max_completion_tokens", "max_tokens")
	// top_logprobs is only valid together with logprobs=true (400 otherwise).
	if !doc.Get("logprobs").Bool() {
		_ = doc.Delete("top_logprobs")
	}
	if tools := doc.Get("tools"); tools.IsArray() && len(tools.Array()) == 0 {
		_ = doc.Delete("tools")
		_ = doc.Delete("tool_choice")
	}
	sanitizeMistralReasoningEffortDoc(doc)
	messages := doc.Get("messages").Array()
	// Pass 1 (forward): normalize OpenAI "developer" role and mid-conversation "system"
	// roles so Mistral's strict union tag ('system'|'user'|'assistant'|'tool') and
	// ordering rule (no 'system' after 'assistant'/'tool') never fail with HTTP 422.
	sawAssistantOrTool := false
	for i, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		path := fmt.Sprintf("messages.%d", i)
		switch role {
		case "assistant", "tool":
			sawAssistantOrTool = true
		case "developer":
			if sawAssistantOrTool {
				_ = doc.Set(path+".role", "user")
			} else {
				_ = doc.Set(path+".role", "system")
			}
			flattenTextOnlyChatContentDoc(doc, path, message.Get("content"))
		case "system":
			if sawAssistantOrTool {
				_ = doc.Set(path+".role", "user")
			}
			flattenTextOnlyChatContentDoc(doc, path, message.Get("content"))
		}
	}

	// Pass 2 (backward): strip assistant reasoning fields rejected by Mistral
	// (mirrors 9router paramSupport.js: ["reasoning_content", "reasoning", "reasoning_details"]).
	messages = doc.Get("messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Get("role").String() != "assistant" {
			continue
		}
		hasReasoning := message.Get("reasoning_content").Exists() ||
			message.Get("reasoning").Exists() ||
			message.Get("reasoning_details").Exists()
		if !hasReasoning {
			continue
		}
		path := fmt.Sprintf("messages.%d", i)
		if isEmptyReasoningOnlyChatAssistant(message) {
			_ = doc.Delete(path)
		} else {
			_ = doc.Delete(path + ".reasoning_content")
			_ = doc.Delete(path + ".reasoning")
			_ = doc.Delete(path + ".reasoning_details")
		}
	}
}

// renameMistralFieldDoc moves from to to unless to is already set; from is always removed.
func renameMistralFieldDoc(doc *ojson.Doc, from, to string) {
	value := doc.Get(from)
	if !value.Exists() {
		return
	}
	if !doc.Get(to).Exists() {
		_ = doc.SetRaw(to, value.Raw)
	}
	_ = doc.Delete(from)
}

// sanitizeMistralReasoningEffortDoc normalizes or strips reasoning_effort for
// Mistral models with strict per-model constraints:
//   - zai-glm-5-3* / glm-5.3* on api.mistral.ai only accept ["low", "high", "max"]
//     (sending "medium" or "none" fails with HTTP 400 code 3051).
//   - non-reasoning partner/code models (deepseek-*, codestral-*, devstral-*,
//     ministral-*) reject reasoning_effort altogether when configured via compat.
func sanitizeMistralReasoningEffortDoc(doc *ojson.Doc) {
	if doc == nil {
		return
	}
	effortRes := doc.Get("reasoning_effort")
	if !effortRes.Exists() {
		return
	}
	model := strings.ToLower(strings.TrimSpace(doc.Get("model").String()))
	effort := strings.ToLower(strings.TrimSpace(effortRes.String()))
	if strings.HasPrefix(model, "zai-glm-5-3") || strings.HasPrefix(model, "glm-5.3") || strings.HasPrefix(model, "glm-5-3") {
		switch effort {
		case "", "none", "off":
			_ = doc.Delete("reasoning_effort")
		case "minimal", "low":
			_ = doc.Set("reasoning_effort", "low")
		case "medium", "high":
			_ = doc.Set("reasoning_effort", "high")
		case "xhigh", "max":
			_ = doc.Set("reasoning_effort", "max")
		default:
			_ = doc.Set("reasoning_effort", "high")
		}
		return
	}
	if strings.HasPrefix(model, "deepseek-") ||
		strings.HasPrefix(model, "codestral-") ||
		strings.HasPrefix(model, "devstral-") ||
		strings.HasPrefix(model, "ministral-") {
		_ = doc.Delete("reasoning_effort")
	}
}

func flattenTextOnlyChatContentDoc(doc *ojson.Doc, msgPath string, content ojson.Result) {
	if doc == nil || !content.IsArray() {
		return
	}
	parts := content.Array()
	if len(parts) == 0 {
		_ = doc.Set(msgPath+".content", "")
		return
	}
	var sb strings.Builder
	for idx, p := range parts {
		if !p.IsObject() {
			return
		}
		ptype := strings.ToLower(strings.TrimSpace(p.Get("type").String()))
		if ptype != "" && ptype != "text" && ptype != "input_text" && ptype != "output_text" {
			return
		}
		if idx > 0 && sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Get("text").String())
	}
	_ = doc.Set(msgPath+".content", sb.String())
}

// Only remove simple reasoning-only turns. Preserve whitespace, multipart
// content, and any other fields, including explicit assistant prefixes.
func isEmptyReasoningOnlyChatAssistant(message ojson.Result) bool {
	content := message.Get("content")
	empty := !content.Exists() || content.Type == ojson.Null ||
		(content.Type == ojson.String && content.String() == "") ||
		(content.IsArray() && len(content.Array()) == 0)
	if !empty {
		return false
	}
	reasoningOnly := true
	message.ForEach(func(key, _ ojson.Result) bool {
		switch key.String() {
		case "role", "content", "reasoning_content", "reasoning", "reasoning_details":
			return true
		default:
			reasoningOnly = false
			return false
		}
	})
	return reasoningOnly
}

// NormalizeMistralChatResponse flattens Mistral's multipart thinking/text
// content blocks (returned by reasoning models such as zai-glm-5-3-flash and
// magistral-* on api.mistral.ai) into the standard OpenAI string content +
// reasoning_content fields on both non-streaming messages and streaming deltas.
func NormalizeMistralChatResponse(body []byte, baseURL string) []byte {
	if len(body) == 0 || !isMistralBaseURL(baseURL) || !ojson.ValidBytes(body) {
		return body
	}
	choices := ojson.GetBytesNoCopy(body, "choices")
	if !choices.IsArray() {
		return body
	}
	choiceArr := choices.Array()
	if len(choiceArr) == 0 {
		return body
	}

	var doc *ojson.Doc
	ensureDoc := func() *ojson.Doc {
		if doc == nil {
			doc = ojson.NewDoc(body)
		}
		return doc
	}

	for i, choice := range choiceArr {
		for _, target := range []string{"message", "delta"} {
			node := choice.Get(target)
			if !node.IsObject() {
				continue
			}
			content := node.Get("content")
			if !content.IsArray() {
				continue
			}
			var thinkingBuf, textBuf strings.Builder
			matched := false
			for _, part := range content.Array() {
				if !part.IsObject() {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
				case "thinking":
					matched = true
					if inner := part.Get("thinking"); inner.IsArray() {
						for _, th := range inner.Array() {
							if th.Type == ojson.String {
								thinkingBuf.WriteString(th.String())
							} else if th.IsObject() {
								thinkingBuf.WriteString(th.Get("text").String())
							}
						}
					} else if inner.Type == ojson.String {
						thinkingBuf.WriteString(inner.String())
					} else if txt := part.Get("text").String(); txt != "" {
						thinkingBuf.WriteString(txt)
					}
				case "text":
					matched = true
					textBuf.WriteString(part.Get("text").String())
				}
			}
			if !matched {
				continue
			}
			d := ensureDoc()
			basePath := fmt.Sprintf("choices.%d.%s", i, target)
			_ = d.Set(basePath+".content", textBuf.String())
			if thinkingBuf.Len() > 0 && !node.Get("reasoning_content").Exists() {
				_ = d.Set(basePath+".reasoning_content", thinkingBuf.String())
			}
		}
	}
	if doc != nil {
		return doc.Bytes()
	}
	return body
}
