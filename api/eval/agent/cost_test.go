package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Arkel-ai/nomos/api/config"
)

func testRelease() ReleaseConfig {
	return ReleaseConfig{
		Name: "test", TariffDate: "2026-10-15",
		Models: ModelSet{Controller: "openai/gpt-6-luna-pro", Review: "openai/gpt-6-luna",
			Extraction: "anthropic/claude-haiku-5.5", Vision: "anthropic/claude-haiku-5.5",
			Embedding: "openai/text-embedding-3-small", Rerank: "cohere/rerank-4-fast"},
		ApprovedReturnedModels: map[string][]string{"openai/gpt-6-luna-pro": {"openai/gpt-6-luna-pro-20261001"}},
		ApprovedDomains:        []string{"droit_du_travail"},
		StepConcurrency:        Concurrency{LLM: 4, DB: 8, OCR: 1, Render: 1},
	}
}

// The actual model a provider ran is checked on every call: an approved
// dated alias passes, anything else is a substitution; a scripted dispatch
// never reached a provider and is not judged.
func TestModelSubstitutionIsNamed(t *testing.T) {
	rc := testRelease()
	ok := []CallRecord{
		{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna-pro", ReturnedModel: "openai/gpt-6-luna-pro-20261001", Status: 200},
		{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna", ReturnedModel: "openai/gpt-6-luna", Status: 200},
		{Path: "/embeddings", RequestedModel: "openai/text-embedding-3-small", ReturnedModel: "text-embedding-3-small", Status: 200},
		{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna-pro", Status: 200, Scripted: true},
		{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna-pro", Status: 503},
	}
	if issues := ModelIssues(rc, ok); len(issues) > 0 {
		t.Fatalf("approved calls were flagged: %v", issues)
	}
	bad := append(ok,
		CallRecord{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna-pro", ReturnedModel: "openai/gpt-5-mini", Status: 200},
		CallRecord{Path: "/chat/completions", RequestedModel: "google/gemini-3.7-flash", ReturnedModel: "google/gemini-3.7-flash", Status: 200},
		CallRecord{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna", Status: 200},
		CallRecord{Path: "/embeddings", RequestedModel: "openai/text-embedding-3-small", ReturnedModel: "text-embedding-3-large", Status: 200},
		CallRecord{Path: "/chat/completions", RequestedModel: "openai/gpt-6-luna-pro", ReturnedModel: "gpt-6-luna", Status: 200},
	)
	issues := ModelIssues(rc, bad)
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"unapproved substitution: requested openai/gpt-6-luna-pro, provider ran openai/gpt-5-mini",
		"unapproved substitution: requested openai/text-embedding-3-small, provider ran text-embedding-3-large",
		"unapproved substitution: requested openai/gpt-6-luna-pro, provider ran gpt-6-luna",
		"requested model google/gemini-3.7-flash is not in the release config", "did not say which model ran"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, issues)
		}
	}
}

// -require-models compares the environment with the release config and
// names every difference before anything is spent.
func TestProductionConfigMismatchIsNamed(t *testing.T) {
	rc := testRelease()
	cfg := config.Config{PlannerModel: "google/gemini-3.7-flash", DrafterModel: "openai/gpt-6-luna",
		ExtractionModel: "anthropic/claude-haiku-5.5", VisionModel: "anthropic/claude-haiku-5.5",
		EmbeddingModel: "openai/text-embedding-3-small", RerankModel: "cohere/rerank-4-fast",
		StepConcurrencyLLM: 4, StepConcurrencyDB: 8, StepConcurrencyOCR: 1, StepConcurrencyRender: 1}
	t.Setenv("WEB_SEARCH_MODEL", "")
	t.Setenv("OPENROUTER_PROVIDER_ALLOWLIST", "")
	problems := CheckProduction(cfg, rc)
	if len(problems) != 1 || !strings.Contains(problems[0], "PLANNER_MODEL") {
		t.Fatalf("problems = %v", problems)
	}
	t.Setenv("WEB_SEARCH_MODEL", "google/gemini-3.7-flash:online")
	cfg.PlannerModel = rc.Models.Controller
	problems = CheckProduction(cfg, rc)
	if len(problems) != 1 || !strings.Contains(problems[0], "WEB_SEARCH_MODEL") {
		t.Fatalf("an :online web model must be named: %v", problems)
	}
}

// The committed release config parses, and a config naming an :online web
// model is refused (owner decision 6).
func TestReleaseConfigIsStrict(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".eval", "release-eval-config.json")
	rc, sha, err := LoadReleaseConfig(path)
	if err != nil {
		t.Fatalf("committed release config: %v", err)
	}
	if rc.Models.Controller != "openai/gpt-6-luna-pro" || rc.Models.Review != "openai/gpt-6-luna" || sha == "" {
		t.Fatalf("the release config does not name the production controller/review models: %+v", rc.Models)
	}
	raw, _ := os.ReadFile(path)
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(strings.Replace(string(raw), `"web_search": ""`, `"web_search": "x/y:online"`, 1)), 0o644)
	if _, _, err := LoadReleaseConfig(bad); err == nil {
		t.Fatal("an :online web model was accepted")
	}
	unknown := filepath.Join(t.TempDir(), "unknown.json")
	os.WriteFile(unknown, []byte(strings.Replace(string(raw), `"name"`, `"surprise": 1, "name"`, 1)), 0o644)
	if _, _, err := LoadReleaseConfig(unknown); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}
