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

// SanitizeMistralChatRequestDoc removes fields rejected by Mistral's Chat
// Completions schema. Run it after built-in request preparation and before
// payload rules so explicit operator configuration retains final precedence.
func SanitizeMistralChatRequestDoc(doc *ojson.Doc, baseURL string) {
	if doc == nil || !isMistralBaseURL(baseURL) {
		return
	}

	_ = doc.Delete("stream_options")
	// Mistral rejects the client_metadata field some clients attach.
	_ = doc.Delete("client_metadata")
	messages := doc.Get("messages").Array()
	// Delete backwards to keep the remaining message indices stable.
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Get("role").String() != "assistant" || !message.Get("reasoning_content").Exists() {
			continue
		}
		path := fmt.Sprintf("messages.%d", i)
		if isEmptyReasoningOnlyChatAssistant(message) {
			_ = doc.Delete(path)
		} else {
			_ = doc.Delete(path + ".reasoning_content")
		}
	}
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
		case "role", "content", "reasoning_content":
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
