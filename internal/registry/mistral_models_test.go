package registry

import (
	"testing"
)

// The embedded and remote catalogs carry no mistral section, so the built-in
// list must be served and reachable through every lookup path.
func TestGetMistralModelsStaticFallback(t *testing.T) {
	models := GetMistralModels()
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	for _, id := range []string{
		"mistral-large-4", "mistral-large-latest", "mistral-medium-latest", "mistral-small-latest",
		"codestral-latest", "devstral-small-latest", "ministral-14b-2512", "ministral-8b-2512",
		"ministral-3b-2512", "zai-glm-5-3", "zai-glm-5-3-flash", "deepseek-v4-1-flash-1017",
	} {
		model := byID[id]
		if model == nil {
			t.Errorf("missing %s", id)
			continue
		}
		if LookupStaticModelInfo(id) == nil || LookupStaticModelInfoByChannel(id, "mistral") == nil {
			t.Errorf("%s is not reachable through static lookups", id)
		}
	}
	if byID["mistral-medium-latest"].Thinking == nil || byID["codestral-latest"].Thinking != nil {
		t.Errorf("thinking support must only be advertised for reasoning models")
	}
	if byID["mistral-large-4"].ContextLength != 1000000 || byID["mistral-large-4"].MaxCompletionTokens != 32768 {
		t.Errorf("mistral-large-4 limits = %+v", byID["mistral-large-4"])
	}
	// Returned models are clones: mutating them must not leak into the list.
	byID["mistral-large-4"].ID = "mutated"
	if GetMistralModels()[0].ID != "mistral-large-4" {
		t.Fatal("GetMistralModels must return clones")
	}
}

func TestGetMistralModelsPrefersCatalogSection(t *testing.T) {
	modelsCatalogStore.mu.Lock()
	previous := modelsCatalogStore.data
	next := *previous
	next.Mistral = []*ModelInfo{{ID: "mistral-from-catalog"}}
	modelsCatalogStore.data = &next
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = previous
		modelsCatalogStore.mu.Unlock()
	})

	models := GetMistralModels()
	if len(models) != 1 || models[0].ID != "mistral-from-catalog" {
		t.Fatalf("models = %+v, want the catalog section", models)
	}
}

func TestValidateModelsCatalogMistralSectionIsOptional(t *testing.T) {
	if err := validateModelsCatalog(&staticModelsJSON{}); err != nil {
		t.Fatalf("missing mistral section must be accepted: %v", err)
	}
	if err := validateModelsCatalog(&staticModelsJSON{Mistral: []*ModelInfo{{ID: "a"}, {ID: "a"}}}); err == nil {
		t.Fatal("duplicate mistral ids must be rejected")
	}
	if err := validateModelsCatalog(&staticModelsJSON{Mistral: []*ModelInfo{nil}}); err == nil {
		t.Fatal("null mistral entries must be rejected")
	}
}

func TestDetectChangedProvidersIncludesMistral(t *testing.T) {
	changed := detectChangedProviders(&staticModelsJSON{}, &staticModelsJSON{Mistral: []*ModelInfo{{ID: "mistral-medium-latest"}}})
	for _, provider := range changed {
		if provider == "mistral" {
			return
		}
	}
	t.Fatalf("changed = %v, want mistral", changed)
}
