package registry

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestModelLifecycleFiltering(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		provider string
		model    *ModelInfo
		visible  bool
	}{
		{"retired haiku", "claude", &ModelInfo{ID: "claude-3-5-haiku-20241022"}, false},
		{"retired alias", "claude", &ModelInfo{ID: "friendly", Name: "claude-3-5-haiku-20241022", UserDefined: true}, false},
		{"oauth alias", "claude", &ModelInfo{ID: "friendly", MetadataModelID: "claude-3-5-haiku-20241022"}, false},
		{"other provider", "antigravity", &ModelInfo{ID: "claude-3-5-haiku-20241022"}, true},
		{"deprecated still works", "claude", &ModelInfo{ID: "claude-sonnet-4-5-20250929", LifecycleStatus: "deprecated"}, true},
		{"unknown model", "claude", &ModelInfo{ID: "future-model"}, true},
		{"retired metadata", "gemini", &ModelInfo{ID: "old", LifecycleStatus: "retired"}, false},
		{"withdrawal date", "xai", &ModelInfo{ID: "old", RetirementDate: "2026-11-17"}, false},
		{"future withdrawal", "xai", &ModelInfo{ID: "current", RetirementDate: "2026-11-18"}, true},
		{"invalid date", "kimi", &ModelInfo{ID: "current", RetirementDate: "unknown"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := filterAvailableModelsAt(test.provider, []*ModelInfo{nil, test.model}, now)
			if (len(got) == 1) != test.visible {
				t.Fatalf("visible = %v, want %v", len(got) == 1, test.visible)
			}
		})
	}
}

func TestModelLifecycleRetirementBoundary(t *testing.T) {
	model := &ModelInfo{ID: "scheduled", RetirementDate: "2026-11-30"}
	deadline := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	if len(filterAvailableModelsAt("gemini", []*ModelInfo{model}, deadline.Add(-time.Nanosecond))) != 1 {
		t.Fatal("model hidden before retirement")
	}
	if len(filterAvailableModelsAt("gemini", []*ModelInfo{model}, deadline)) != 0 {
		t.Fatal("model visible at retirement")
	}
}

func TestModelLifecycleRegistrationPreservesOtherProviders(t *testing.T) {
	r := &ModelRegistry{
		models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string),
		clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string),
		mutex: &sync.RWMutex{},
	}
	model := &ModelInfo{ID: "claude-3-5-haiku-20241022"}
	r.RegisterClientWithCandidates("anthropic", "claude", []*ModelInfo{model}, []*ModelInfo{model})
	if len(r.GetAvailableModelDefinitions()) != 0 || len(r.GetModelCandidatesForClient("anthropic")) != 0 {
		t.Fatal("retired model registered or exposed as a candidate")
	}
	r.RegisterClient("partner", "antigravity", []*ModelInfo{model})
	got := r.GetAvailableModelDefinitions()
	if len(got) != 1 || len(got[0].Providers) != 1 || got[0].Providers[0] != "antigravity" {
		t.Fatalf("partner registration lost: %+v", got)
	}
}

func TestEmbeddedClaudeCatalogContainsNoRetiredModels(t *testing.T) {
	var catalog staticModelsJSON
	if errUnmarshal := json.Unmarshal(embeddedModelsJSON, &catalog); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	// Use the catalog cleanup date so future retirements do not make this test flaky.
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	for _, model := range catalog.Claude {
		if len(filterAvailableModelsAt("claude", []*ModelInfo{model}, now)) == 0 {
			t.Errorf("embedded catalog contains retired model %q", model.ID)
		}
	}
	foundDeprecated := false
	for _, model := range catalog.Claude {
		if model.ID == "claude-sonnet-4-5-20250929" {
			foundDeprecated = true
		}
	}
	if !foundDeprecated {
		t.Fatal("cleanup removed deprecated but still available Sonnet 4.5")
	}
}

func TestClaudeCatalogOmitsRetiredModels(t *testing.T) {
	for _, model := range GetClaudeModels() {
		if model.ID == "claude-3-5-haiku-20241022" {
			t.Fatal("embedded or refreshed catalog resurrected retired Haiku")
		}
	}
}
