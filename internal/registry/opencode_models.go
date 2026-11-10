package registry

import "strings"

const opencodeModelsCreated int64 = 1789257600

var (
	opencodeStandardReasoningLevels = []string{"low", "medium", "high"}
	opencodeDeepSeekReasoningLevels = []string{"low", "high", "max"}
	opencodeHighReasoningLevels     = []string{"low", "medium", "high", "xhigh"}
)

var opencodeResponsesOnlyModels = map[string]bool{
	"grok-4.7":                   true,
	"grok-4.6":                   true,
	"grok-4.5":                   true,
	"gpt-5.6-luna":               true,
	"gpt-6-luna":                 true,
	"muse-spark-1.2-contributor": true,
	"muse-spark-1.3-contributor": true,
}

var opencodeDualResponsesModels = map[string]bool{
	"deepseek-v4-pro":              true,
	"deepseek-v4-flash":            true,
	"deepseek-v4-flash-vision-exp": true,
	"deepseek-v4.1-flash":          true,
}

func normalizeOpencodeModelID(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if openParen := strings.LastIndex(key, "("); openParen >= 0 && strings.HasSuffix(key, ")") {
		key = strings.TrimSpace(key[:openParen])
	}
	if slash := strings.LastIndex(key, "/"); slash >= 0 && slash < len(key)-1 {
		key = strings.TrimSpace(key[slash+1:])
	}
	return key
}

// IsOpencodeResponsesOnlyModel reports whether an OpenCode Go model requires
// the OpenAI Responses API (/zen/go/v1/responses) rather than /chat/completions.
func IsOpencodeResponsesOnlyModel(model string) bool {
	key := normalizeOpencodeModelID(model)
	if key == "" {
		return false
	}
	if opencodeResponsesOnlyModels[key] {
		return true
	}
	return strings.HasPrefix(key, "gpt-") ||
		strings.HasPrefix(key, "grok-") ||
		strings.HasPrefix(key, "muse-spark-")
}

// OpencodeSupportsResponses reports whether an OpenCode Go model supports the
// OpenAI Responses API endpoint (/zen/go/v1/responses).
func OpencodeSupportsResponses(model string) bool {
	if IsOpencodeResponsesOnlyModel(model) {
		return true
	}
	key := normalizeOpencodeModelID(model)
	if opencodeDualResponsesModels[key] {
		return true
	}
	return strings.HasPrefix(key, "deepseek-v4")
}

// OpencodeUpstreamRoute returns the default upstream wire route for an OpenCode Go
// model: "responses" for Responses-only models (gpt-*, grok-*, muse-spark-*) and
// "chat" for all other models.
func OpencodeUpstreamRoute(model string) string {
	if IsOpencodeResponsesOnlyModel(model) {
		return "responses"
	}
	return "chat"
}

func opencodeModel(id, displayName string, contextLength, maxCompletionTokens int, levels []string, zeroAllowed bool, vision bool) *ModelInfo {
	inputModalities := []string{"text"}
	if vision {
		inputModalities = []string{"text", "image"}
	}
	info := &ModelInfo{
		ID:                        id,
		Object:                    "model",
		Created:                   opencodeModelsCreated,
		OwnedBy:                   "opencode",
		Type:                      "opencode",
		DisplayName:               displayName,
		Name:                      id,
		Description:               displayName + " via OpenCode Go.",
		ContextLength:             contextLength,
		MaxCompletionTokens:       maxCompletionTokens,
		SupportedInputModalities:  inputModalities,
		SupportedOutputModalities: []string{"text"},
	}
	if len(levels) > 0 {
		info.Thinking = &ThinkingSupport{
			ZeroAllowed: zeroAllowed,
			Levels:      append([]string(nil), levels...),
		}
	}
	return info
}

var staticOpencodeModels = []*ModelInfo{
	// Z.ai GLM family
	opencodeModel("glm-5.3-flash", "GLM-5.3 Flash", 204800, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("glm-5.3", "GLM-5.3", 204800, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("glm-5.2", "GLM-5.2", 204800, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("glm-5.1", "GLM-5.1", 204800, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("glm-5", "GLM-5", 204800, 131072, opencodeStandardReasoningLevels, true, false),

	// Moonshot Kimi family
	opencodeModel("kimi-k2.7-code", "Kimi K2.7 Code", 262144, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("kimi-k2.6", "Kimi K2.6", 262144, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("kimi-k2.5", "Kimi K2.5", 262144, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("kimi-k3", "Kimi K3", 1048576, 384000, opencodeStandardReasoningLevels, false, true),

	// DeepSeek family
	opencodeModel("deepseek-v4-pro", "DeepSeek V4 Pro", 1000000, 384000, opencodeDeepSeekReasoningLevels, false, false),
	opencodeModel("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, 384000, opencodeDeepSeekReasoningLevels, false, false),
	opencodeModel("deepseek-v4-flash-vision-exp", "DeepSeek V4 Flash Vision Exp", 1000000, 384000, opencodeDeepSeekReasoningLevels, false, true),
	opencodeModel("deepseek-v4.1-flash", "DeepSeek V4.1 Flash", 1000000, 384000, opencodeDeepSeekReasoningLevels, true, false),
	opencodeModel("deepseek-flash", "DeepSeek Flash", 262144, 65536, opencodeDeepSeekReasoningLevels, true, false),

	// Meituan LongCat family
	opencodeModel("longcat-2.0", "LongCat 2.0", 262144, 98304, opencodeStandardReasoningLevels, false, false),
	opencodeModel("longcat-2.5-preview-free", "LongCat 2.5 Preview (Free)", 262144, 98304, opencodeStandardReasoningLevels, false, false),

	// Xiaomi MiMo family
	opencodeModel("mimo-v2.6-flash", "MiMo V2.6 Flash", 262144, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("mimo-v2.6-pro", "MiMo V2.6 Pro", 1048576, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("mimo-v2.5", "MiMo V2.5", 262144, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("mimo-v2.5-pro", "MiMo V2.5 Pro", 1048576, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("mimo-v2-pro", "MiMo V2 Pro", 1048576, 131072, opencodeStandardReasoningLevels, true, false),
	opencodeModel("mimo-v2-omni", "MiMo V2 Omni", 262144, 65536, opencodeStandardReasoningLevels, true, true),

	// MiniMax family
	opencodeModel("minimax-m3", "MiniMax M3", 1048576, 131072, opencodeStandardReasoningLevels, false, false),
	opencodeModel("minimax-m2.7", "MiniMax M2.7", 204800, 131072, opencodeStandardReasoningLevels, false, false),
	opencodeModel("minimax-m2.5", "MiniMax M2.5", 204800, 131072, opencodeStandardReasoningLevels, false, false),
	opencodeModel("space-bunny-free", "Space Bunny (Free)", 204800, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("ox-alpha-free", "Ox Alpha (Free)", 262144, 65536, opencodeStandardReasoningLevels, true, false),

	// Alibaba Qwen family
	opencodeModel("qwen3.8-max", "Qwen 3.8 Max", 1000000, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("qwen3.8-flash", "Qwen 3.8 Flash", 1000000, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("qwen3.7-max", "Qwen 3.7 Max", 262144, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("qwen3.7-plus", "Qwen 3.7 Plus", 1000000, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("qwen3.6-plus", "Qwen 3.6 Plus", 1000000, 65536, opencodeStandardReasoningLevels, true, true),
	opencodeModel("qwen3.5-plus", "Qwen 3.5 Plus", 1000000, 65536, opencodeStandardReasoningLevels, true, true),

	// Tencent Hunyuan (HY) & Omen family
	opencodeModel("hy4-preview", "Hunyuan 4 Preview", 262144, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("hy3", "Hunyuan 3.0", 262144, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("hy3-preview", "Hunyuan 3.0 Preview", 262144, 65536, opencodeStandardReasoningLevels, true, false),
	opencodeModel("omen-alpha", "Omen Alpha", 262144, 65536, opencodeStandardReasoningLevels, true, false),

	// xAI Grok family (Responses API only on OpenCode Go)
	opencodeModel("grok-4.7", "Grok 4.7", 262144, 131072, opencodeHighReasoningLevels, false, true),
	opencodeModel("grok-4.6", "Grok 4.6", 262144, 131072, opencodeHighReasoningLevels, false, true),
	opencodeModel("grok-4.5", "Grok 4.5", 262144, 65536, opencodeStandardReasoningLevels, false, true),

	// OpenAI GPT Luna family (Responses API only on OpenCode Go)
	opencodeModel("gpt-5.6-luna", "GPT-5.6 Luna", 400000, 128000, opencodeHighReasoningLevels, true, true),
	opencodeModel("gpt-6-luna", "GPT-6 Luna", 400000, 128000, opencodeHighReasoningLevels, true, true),

	// Meta Muse Spark family (Responses API only on OpenCode Go)
	opencodeModel("muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor", 262144, 65536, opencodeStandardReasoningLevels, false, true),
	opencodeModel("muse-spark-1.3-contributor", "Muse Spark 1.3 Contributor", 262144, 65536, opencodeStandardReasoningLevels, false, true),
}

// GetOpencodeModels returns the model definitions served for OpenCode Go
// credentials. An "opencode" section in the active models catalog takes
// precedence; otherwise the built-in static catalog is returned.
func GetOpencodeModels() []*ModelInfo {
	if data := getModels(); data != nil && len(data.Opencode) > 0 {
		return cloneModelInfos(data.Opencode)
	}
	return cloneModelInfos(staticOpencodeModels)
}
