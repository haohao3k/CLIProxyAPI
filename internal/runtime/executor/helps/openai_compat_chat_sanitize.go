package helps

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	currentTurnImageOmittedPlaceholder  = "[image omitted: model has no vision support]"
	previousTurnImageOmittedPlaceholder = "[Previous image omitted from context.]"
)

func isOpenAINativeHost(baseURL string) bool {
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.com") || strings.HasSuffix(host, ".openai.azure.com")
}

func isGroqOrCerebrasBaseURL(baseURL string) bool {
	parsed, errParse := url.Parse(strings.TrimSpace(baseURL))
	if errParse != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return host == "groq.com" || strings.HasSuffix(host, ".groq.com") ||
		host == "cerebras.ai" || strings.HasSuffix(host, ".cerebras.ai")
}

// SanitizeOpenAICompatChatRequestDoc ports 9router's Chat Completions request
// sanitization layers (filterToOpenAIFormat, stripContinuityFields, paramSupport,
// DeepSeek tool deduplication, and modality stripping) onto non-Responses chat payloads.
func SanitizeOpenAICompatChatRequestDoc(
	doc *ojson.Doc,
	provider string,
	compat *config.OpenAICompatibility,
	baseURL string,
	baseModel string,
	modelInfo *registry.ModelInfo,
) {
	if doc == nil {
		return
	}

	// 1. Delete empty tools: [] (9router open-sse/translator/formats/openai.js:81-83).
	// Strict gateways (Qwen, Z.ai, Mistral, LiteLLM) reject "tools": [] with HTTP 400.
	if tools := doc.Get("tools"); tools.IsArray() && len(tools.Array()) == 0 {
		_ = doc.Delete("tools")
		_ = doc.Delete("tool_choice")
	}

	// 2. Deduplicate same-name tools for DeepSeek models (9router toolDeduper.js:66-74).
	if strings.Contains(strings.ToLower(baseModel), "deepseek") {
		deduplicateChatToolsDoc(doc)
	}

	// 3. Named third-party openai-compatibility entries (other than native OpenAI)
	// also normalize role="developer" -> "system". Bare unnamed test servers (compat == nil)
	// are preserved unless matched by ShouldNormalizeChatDeveloperRole.
	if compat != nil && strings.TrimSpace(compat.Name) != "" &&
		!strings.EqualFold(strings.TrimSpace(compat.Name), "openai") &&
		!isOpenAINativeHost(baseURL) {
		NormalizeChatDeveloperRoleDoc(doc)
	}

	// 4. Strip Responses continuity fields ("encrypted_content", "reasoning_encrypted_content")
	// from messages[] (9router chatCore.js:52-61), and strip assistant reasoning fields on
	// Groq/Cerebras (9router paramSupport.js:30,32).
	stripGroqCerebrasReasoning := isGroqOrCerebrasBaseURL(baseURL)
	if messages := doc.Get("messages"); messages.IsArray() {
		arr := messages.Array()
		for i, msg := range arr {
			if !msg.IsObject() {
				continue
			}
			path := fmt.Sprintf("messages.%d", i)
			if msg.Get("encrypted_content").Exists() {
				_ = doc.Delete(path + ".encrypted_content")
			}
			if msg.Get("reasoning_encrypted_content").Exists() {
				_ = doc.Delete(path + ".reasoning_encrypted_content")
			}
			if stripGroqCerebrasReasoning && strings.EqualFold(msg.Get("role").String(), "assistant") {
				_ = doc.Delete(path + ".reasoning_content")
				_ = doc.Delete(path + ".reasoning")
				_ = doc.Delete(path + ".reasoning_details")
			}
		}
	}

	// 5. Strip unsupported image modalities when target model explicitly lacks vision
	// (9router open-sse/translator/concerns/modality.js).
	StripUnsupportedImageModalitiesDoc(doc, false, baseModel, provider, modelInfo)
}

func deduplicateChatToolsDoc(doc *ojson.Doc) {
	tools := doc.Get("tools")
	if !tools.IsArray() {
		return
	}
	arr := tools.Array()
	if len(arr) <= 1 {
		return
	}
	seen := make(map[string]struct{}, len(arr))
	keptRaw := make([]string, 0, len(arr))
	changed := false
	for _, item := range arr {
		name := strings.TrimSpace(item.Get("function.name").String())
		if name == "" {
			name = strings.TrimSpace(item.Get("name").String())
		}
		if name != "" {
			if _, dup := seen[name]; dup {
				changed = true
				continue
			}
			seen[name] = struct{}{}
		}
		keptRaw = append(keptRaw, item.Raw)
	}
	if !changed {
		return
	}
	_ = doc.SetRaw("tools", "["+strings.Join(keptRaw, ",")+"]")
}

func lookupModelInfoForProvider(baseModel, provider string) *registry.ModelInfo {
	baseModel = strings.TrimSpace(baseModel)
	if baseModel == "" {
		return nil
	}
	prov := strings.ToLower(strings.TrimSpace(provider))
	if info := registry.LookupModelInfo(baseModel, prov); info != nil {
		return info
	}
	trimmedProv := strings.TrimPrefix(prov, "openai-compatible-")
	if trimmedProv != "" && trimmedProv != prov {
		if info := registry.LookupModelInfo(baseModel, trimmedProv); info != nil {
			return info
		}
		if info := registry.LookupStaticModelInfoByChannel(baseModel, trimmedProv); info != nil {
			return info
		}
	}
	return nil
}

func modelExplicitlyLacksVision(info *registry.ModelInfo) bool {
	if info == nil || len(info.SupportedInputModalities) == 0 {
		return false
	}
	if info.UserDefined && !info.ExplicitInputModalities {
		return false
	}
	for _, m := range info.SupportedInputModalities {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "image", "images", "vision":
			return false
		}
	}
	return true
}

// StripUnsupportedImageModalitiesDoc replaces image blocks with text placeholders
// when the resolved target model explicitly declares input modalities without image/vision.
func StripUnsupportedImageModalitiesDoc(
	doc *ojson.Doc,
	responsesWire bool,
	baseModel string,
	provider string,
	modelInfo *registry.ModelInfo,
) {
	if doc == nil {
		return
	}
	info := modelInfo
	if info == nil {
		info = lookupModelInfoForProvider(baseModel, provider)
	}
	if !modelExplicitlyLacksVision(info) {
		return
	}

	arrayKey := "messages"
	textPartType := "text"
	if responsesWire {
		arrayKey = "input"
		textPartType = "input_text"
	}
	list := doc.Get(arrayKey)
	if !list.IsArray() {
		return
	}
	items := list.Array()
	// Locate start of trailing user turn (after last assistant/tool item).
	lastNonUserIdx := -1
	for i, item := range items {
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if role == "assistant" || role == "model" || role == "tool" ||
			itemType == "function_call" || itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			lastNonUserIdx = i
		}
	}

	for i, item := range items {
		if !item.IsObject() {
			continue
		}
		isCurrentTurn := i > lastNonUserIdx
		placeholder := previousTurnImageOmittedPlaceholder
		if isCurrentTurn {
			placeholder = currentTurnImageOmittedPlaceholder
		}
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if responsesWire && (itemType == "input_image" || itemType == "image" || itemType == "image_url") {
			replacement := ojson.NewDocString(`{}`)
			_ = replacement.Set("type", "message")
			_ = replacement.Set("role", "user")
			_ = replacement.Set("content.0.type", textPartType)
			_ = replacement.Set("content.0.text", placeholder)
			_ = doc.SetRawBytes(fmt.Sprintf("%s.%d", arrayKey, i), replacement.Bytes())
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		parts := content.Array()
		changed := false
		for j, part := range parts {
			if !part.IsObject() {
				continue
			}
			ptype := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			if ptype == "image_url" || ptype == "input_image" || ptype == "image" {
				partPath := fmt.Sprintf("%s.%d.content.%d", arrayKey, i, j)
				rep := ojson.NewDocString(`{}`)
				_ = rep.Set("type", textPartType)
				_ = rep.Set("text", placeholder)
				_ = doc.SetRawBytes(partPath, rep.Bytes())
				changed = true
			}
		}
		_ = changed
	}
}
