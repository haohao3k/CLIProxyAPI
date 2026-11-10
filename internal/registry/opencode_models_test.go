package registry

import "testing"

func TestGetOpencodeModelsAndRouting(t *testing.T) {
	models := GetOpencodeModels()
	if len(models) < 42 {
		t.Fatalf("GetOpencodeModels() returned %d models, want >= 42", len(models))
	}
	byID := make(map[string]*ModelInfo, len(models))
	for _, m := range models {
		if m == nil || m.ID == "" {
			t.Fatalf("unexpected nil or empty model in GetOpencodeModels: %#v", m)
		}
		byID[m.ID] = m
	}

	requiredIDs := []string{
		"deepseek-flash",
		"glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1", "glm-5",
		"kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "kimi-k3",
		"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp", "deepseek-v4.1-flash",
		"longcat-2.0",
		"mimo-v2.6-flash", "mimo-v2.6-pro", "mimo-v2.5", "mimo-v2.5-pro", "mimo-v2-pro", "mimo-v2-omni",
		"minimax-m3", "minimax-m2.7", "minimax-m2.5", "space-bunny-free",
		"qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus",
		"hy4-preview", "hy3", "hy3-preview", "omen-alpha",
		"grok-4.7", "grok-4.6", "grok-4.5",
		"gpt-5.6-luna", "gpt-6-luna",
		"muse-spark-1.2-contributor", "muse-spark-1.3-contributor",
	}
	for _, id := range requiredIDs {
		if _, ok := byID[id]; !ok {
			t.Errorf("GetOpencodeModels() missing required model %q", id)
		}
	}

	responsesOnly := []string{
		"grok-4.7", "grok-4.6", "grok-4.5",
		"gpt-5.6-luna", "gpt-6-luna", "gpt-6-luna(medium)", "opencode/gpt-6-luna",
		"muse-spark-1.2-contributor", "muse-spark-1.3-contributor",
	}
	for _, id := range responsesOnly {
		if !IsOpencodeResponsesOnlyModel(id) {
			t.Errorf("IsOpencodeResponsesOnlyModel(%q) = false, want true", id)
		}
		if !OpencodeSupportsResponses(id) {
			t.Errorf("OpencodeSupportsResponses(%q) = false, want true", id)
		}
		if got := OpencodeUpstreamRoute(id); got != "responses" {
			t.Errorf("OpencodeUpstreamRoute(%q) = %q, want responses", id, got)
		}
	}

	chatModels := []string{"glm-5.3", "glm-5.3-flash", "kimi-k2.7-code", "minimax-m3", "qwen3.8-max"}
	for _, id := range chatModels {
		if IsOpencodeResponsesOnlyModel(id) {
			t.Errorf("IsOpencodeResponsesOnlyModel(%q) = true, want false", id)
		}
		if got := OpencodeUpstreamRoute(id); got != "chat" {
			t.Errorf("OpencodeUpstreamRoute(%q) = %q, want chat", id, got)
		}
	}

	if !OpencodeSupportsResponses("deepseek-v4-pro") {
		t.Error("expected OpencodeSupportsResponses(deepseek-v4-pro) = true")
	}
	if IsOpencodeResponsesOnlyModel("deepseek-v4-pro") {
		t.Error("expected IsOpencodeResponsesOnlyModel(deepseek-v4-pro) = false")
	}
}
