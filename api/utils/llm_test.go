package utils

import (
	"context"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
)

// These tests hit the real gateway and the real database, because the two
// things worth proving here cannot be proven against a mock: that bge-m3
// really returns the 1024 dimensions the halfvec column commits to, and that
// every call really lands a reconciled llm_calls row. They skip
// (never fail) when api/.env has no key or no DSN.

func loadTestEnv(t *testing.T) config.Config {
	t.Helper()
	_ = godotenv.Load("../.env")
	cfg := config.Load()
	if cfg.OpenRouterAPIKey == "" {
		t.Skip("OPENROUTER_API_KEY not set — skipping live gateway test")
	}
	if DB == nil && cfg.DatabaseURL != "" {
		initDB(cfg.DatabaseURL)
	}
	if DB == nil {
		t.Skip("DATABASE_URL not set — skipping (the cost path is the point of these tests)")
	}
	return cfg
}

// countRows is how the cost path is asserted: not "the function returned", but
// "the row is in Postgres".
func countRows(t *testing.T, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := DB.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestLLMEmbed(t *testing.T) {
	cfg := loadTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	vectors, err := Embed(ctx, cfg.EmbeddingModel, []string{
		"Le licenciement doit reposer sur une cause réelle et sérieuse.",
		"L'article L. 1234-1 du code du travail fixe le préavis.",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vectors))
	}
	for i, v := range vectors {
		if len(v) != cfg.EmbeddingDims {
			t.Fatalf("vector %d has %d dimensions, want %d", i, len(v), cfg.EmbeddingDims)
		}
	}
	t.Logf("Embed: %d vectors of %d dimensions (model=%s)", len(vectors), len(vectors[0]), cfg.EmbeddingModel)

	if n := countRows(t, "llm_calls", "purpose = ?", types.LLMPurposeEmbed); n == 0 {
		t.Fatal("no llm_calls row recorded for the embedding call")
	}
}

func TestLLMRerank(t *testing.T) {
	cfg := loadTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	docs := []string{
		"La recette de la tarte aux pommes demande trois œufs et du beurre.",
		"Le licenciement doit être justifié par une cause réelle et sérieuse (C. trav., art. L. 1232-1).",
		"Le train pour Bordeaux part de la voie 12.",
	}
	hits, err := Rerank(ctx, cfg.RerankModel, "licenciement sans cause réelle et sérieuse", docs, 3)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d", len(hits))
	}
	if hits[0].Index != 1 {
		t.Fatalf("expected the legal sentence (index 1) first, got index %d", hits[0].Index)
	}
	for _, h := range hits {
		t.Logf("Rerank: index=%d score=%.6f", h.Index, h.Score)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores are not sorted descending: %v", hits)
	}
}

// pieceQualification is a SERVER-OWNED schema: the shape is decided here, in
// code, never by the model (§4.3).
type pieceQualification struct {
	TypePiece string `json:"type_piece"`
	DateActe  string `json:"date_acte"`
	Emetteur  string `json:"emetteur"`
	Titre     string `json:"titre"`
}

var pieceSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"type_piece", "date_acte", "emetteur", "titre"},
	"properties": map[string]any{
		"type_piece": map[string]any{"type": "string"},
		"date_acte":  map[string]any{"type": "string"},
		"emetteur":   map[string]any{"type": "string"},
		"titre":      map[string]any{"type": "string"},
	},
}

func TestLLMChatJSON(t *testing.T) {
	cfg := loadTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var out pieceQualification
	err := ChatJSON(ctx, ChatOpts{
		Model:   cfg.ExtractionModel,
		Purpose: types.LLMPurposeQualify,
		// Static identity, XML-tagged sections.
		System: "<role>Tu es un assistant qui qualifie les pièces d'un dossier " +
			"contentieux français. Tu ne commentes pas le droit, tu décris la pièce.</role>\n" +
			"<tasks>Identifier le type de pièce, sa date, son émetteur et un titre court.</tasks>\n" +
			"<output>Un unique document JSON conforme au schéma imposé, sans texte autour. " +
			"date_acte au format JJ/MM/AAAA.</output>",
		User: "Pièce : lettre de licenciement pour faute grave, datée du 12 mars 2024, " +
			"adressée par la société ACME SAS à Monsieur Dupont, salarié.",
		MaxTokens: 2000,
		TenantID:  "test-tenant",
	}, pieceSchema, &out)
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if out.TypePiece == "" || out.DateActe == "" || out.Emetteur == "" || out.Titre == "" {
		t.Fatalf("incomplete extraction: %+v", out)
	}
	t.Logf("ChatJSON: %+v", out)

	if n := countRows(t, "llm_calls", "purpose = ? AND usage_status = ?",
		types.LLMPurposeQualify, types.UsageStatusKnown); n == 0 {
		t.Fatal("no llm_calls row with recorded usage for the qualification call")
	}
}

// TestLLMSchemaValidation exercises the validator without spending a token.
// These are the failure modes the template's "regex the first JSON blob"
// extractor turns into a plausible-looking wrong answer.
func TestLLMSchemaValidation(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid", `{"type_piece":"lettre","date_acte":"12/03/2024","emetteur":"ACME","titre":"x"}`, false},
		{"missing required field", `{"type_piece":"lettre","date_acte":"12/03/2024","emetteur":"ACME"}`, true},
		{"unknown property", `{"type_piece":"l","date_acte":"d","emetteur":"e","titre":"t","extra":1}`, true},
		{"wrong type", `{"type_piece":42,"date_acte":"d","emetteur":"e","titre":"t"}`, true},
		{"prose around the json", "Voici le résultat :\n{\"type_piece\":\"l\"}", true},
		{"trailing content", `{"type_piece":"l","date_acte":"d","emetteur":"e","titre":"t"} et voilà`, true},
		{"empty", "  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out pieceQualification
			err := decodeStrict([]byte(tc.raw), pieceSchema, &out)
			if tc.wantErr && err == nil {
				t.Fatalf("expected a validation error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestLLMFailedCallIsRecorded is the §4.6(c) contract: a call the gateway
// rejected still leaves a durable row, with usage_status=unknown and no
// cost — there is nothing auditable to bill. It also pins the retry
// policy: a 4xx is deterministic, so it must be dispatched exactly once.
func TestLLMFailedCallIsRecorded(t *testing.T) {
	loadTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := Chat(ctx, ChatOpts{
		Model:    "nomos/does-not-exist",
		Purpose:  types.LLMPurposeChat,
		User:     "ping",
		TenantID: "test-tenant",
	})
	if err == nil {
		t.Fatal("expected the bogus model to fail")
	}
	if IsRetryable(err) {
		t.Fatalf("a rejected model is deterministic and must not be retryable: %v", err)
	}
	t.Logf("Failed call: execution_id=%s err=%v", res.ExecutionID, err)

	if n := countRows(t, "llm_calls", "execution_id = ? AND state = ? AND usage_status = ?",
		res.ExecutionID, types.LLMCallStateFailed, types.UsageStatusUnknown); n != 1 {
		t.Fatalf("expected 1 failed llm_calls row with unknown usage, got %d", n)
	}
	if n := countRows(t, "llm_calls", "execution_id = ? AND cost_nano_usd <> 0", res.ExecutionID); n != 0 {
		t.Fatal("a failed call must not carry a cost")
	}
}
