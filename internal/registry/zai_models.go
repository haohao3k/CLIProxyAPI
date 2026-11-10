package registry

// Z.ai GLM Coding Plan catalog.
//
// The catalog is primarily sourced from the "zai" section of models.json so a
// refreshed catalog can update it without a rebuild. Release builds and the
// remote model updater replace models.json with the upstream catalog, which
// does not carry a "zai" section; staticZAIModels keeps the provider usable in
// that case.

const zaiModelsCreated = 1785600000

// zaiThinkingLevels models GLM's binary thinking switch on the OpenAI
// endpoint: "none" disables thinking, any other level enables it.
var zaiThinkingLevels = []string{"none", "low", "medium", "high"}

func zaiModel(id, displayName string, contextLength, maxCompletion int, vision bool) *ModelInfo {
	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
	}
	return &ModelInfo{
		ID:                        id,
		Object:                    "model",
		Created:                   zaiModelsCreated,
		OwnedBy:                   "zai",
		Type:                      "zai",
		DisplayName:               displayName,
		ContextLength:             contextLength,
		MaxCompletionTokens:       maxCompletion,
		SupportedInputModalities:  inputs,
		SupportedOutputModalities: []string{"text"},
		Thinking:                  &ThinkingSupport{Levels: append([]string(nil), zaiThinkingLevels...), ZeroAllowed: true},
	}
}

var staticZAIModels = []*ModelInfo{
	zaiModel("glm-5.3", "GLM 5.3", 200000, 128000, false),
	zaiModel("glm-5.3-flash", "GLM 5.3 Flash (Vision)", 200000, 128000, true),
	zaiModel("glm-5.2", "GLM 5.2", 200000, 128000, false),
	zaiModel("glm-5.1", "GLM 5.1", 200000, 128000, false),
	zaiModel("glm-5-turbo", "GLM 5 Turbo", 200000, 128000, false),
	zaiModel("glm-5", "GLM 5", 200000, 128000, false),
	zaiModel("glm-4.7", "GLM 4.7", 200000, 128000, false),
	zaiModel("glm-4.6v", "GLM 4.6V (Vision)", 128000, 32768, true),
}

// GetZAIModels returns the Z.ai GLM Coding Plan model definitions.
func GetZAIModels() []*ModelInfo {
	if m := getModels(); m != nil && len(m.ZAI) > 0 {
		return cloneModelInfos(m.ZAI)
	}
	return cloneModelInfos(staticZAIModels)
}
