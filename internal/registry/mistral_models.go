package registry

// mistralModelsCreated is the creation timestamp advertised for the built-in
// Mistral catalog entries.
const mistralModelsCreated int64 = 1787000000

// mistralReasoningLevels mirrors the reasoning efforts Mistral reasoning models
// accept through the OpenAI-compatible reasoning_effort field.
var mistralReasoningLevels = []string{"none", "low", "medium", "high"}

// staticMistralModels is the built-in Mistral model list served for Mistral
// credentials (API key import or Mistral Vibe browser sign-in). It is used
// whenever the active models catalog has no "mistral" section, which is the
// case for the remote catalog and for builds that replace models.json, so a
// Mistral credential always advertises models in /v1/models.
//
// The list mirrors the 9router Mistral provider registry. Embedding models are
// omitted because the OpenAI-compatibility executor serves chat completions.
var staticMistralModels = []*ModelInfo{
	mistralModel("mistral-large-4", "Mistral Large 4 Preview", "Mistral flagship reasoning model (preview)", 1000000, 32768, true),
	mistralModel("mistral-large-latest", "Mistral Large 3", "Mistral large general-purpose model, latest alias", 262144, 0, false),
	mistralModel("mistral-medium-latest", "Mistral Medium 3.5", "Mistral medium reasoning model, latest alias", 262144, 0, true),
	mistralModel("mistral-small-latest", "Mistral Small 4", "Mistral small reasoning model, latest alias", 256000, 0, true),
	mistralModel("codestral-latest", "Codestral", "Mistral code completion model, latest alias", 256000, 0, false),
	mistralModel("devstral-small-latest", "Devstral Small", "Mistral coding-agent small model, latest alias", 128000, 0, false),
	mistralModel("ministral-14b-2512", "Ministral 3 14B", "Mistral edge model, 14B", 262144, 0, false),
	mistralModel("ministral-8b-2512", "Ministral 3 8B", "Mistral edge model, 8B", 262144, 0, false),
	mistralModel("ministral-3b-2512", "Ministral 3 3B", "Mistral edge model, 3B", 262144, 0, false),
	mistralModel("zai-glm-5-3", "GLM 5.3 (Mistral)", "Z.ai GLM 5.3 served by Mistral", 1048576, 128000, true),
	mistralModel("zai-glm-5-3-flash", "GLM 5.3 Flash", "Z.ai GLM 5.3 Flash served by Mistral", 1048576, 128000, true),
	mistralModel("deepseek-v4-1-flash-1017", "DeepSeek V4.1 Flash (Mistral)", "DeepSeek V4.1 Flash served by Mistral", 128000, 0, false),
}

func mistralModel(id, displayName, description string, contextLength, maxCompletionTokens int, reasoning bool) *ModelInfo {
	info := &ModelInfo{
		ID:                        id,
		Object:                    "model",
		Created:                   mistralModelsCreated,
		OwnedBy:                   "mistral",
		Type:                      "openai-compatibility",
		DisplayName:               displayName,
		Name:                      id,
		Description:               description,
		ContextLength:             contextLength,
		MaxCompletionTokens:       maxCompletionTokens,
		SupportedInputModalities:  []string{"text"},
		SupportedOutputModalities: []string{"text"},
	}
	if reasoning {
		info.Thinking = &ThinkingSupport{ZeroAllowed: true, Levels: append([]string(nil), mistralReasoningLevels...)}
	}
	return info
}

// GetMistralModels returns the model definitions served for Mistral
// credentials. A "mistral" section in the active models catalog takes
// precedence; otherwise the built-in list is returned. Mistral is served
// through the OpenAI-compatibility executor, so an openai-compatibility entry
// named "mistral" in config.yaml overrides both.
func GetMistralModels() []*ModelInfo {
	if data := getModels(); data != nil && len(data.Mistral) > 0 {
		return cloneModelInfos(data.Mistral)
	}
	return cloneModelInfos(staticMistralModels)
}
