package utils

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
)

// ChatTools against a scripted OpenRouter and the real database: what is
// worth proving is SQL (the dispatch is recorded before it leaves, reconciled
// with its cost and attribution after) and wire shape (one attempt, native
// tool calls both ways). No real gateway is called.

func toolTestDB(t *testing.T) string {
	t.Helper()
	_ = godotenv.Load("../.env")
	cfg := config.Load()
	if DB == nil && cfg.DatabaseURL != "" {
		initDB(cfg.DatabaseURL)
	}
	if DB == nil {
		t.Skip("DATABASE_URL not set — the accounting path is the point of these tests")
	}
	tenant := "test-tenant-llmtools-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		DB.Exec(`DELETE FROM llm_calls WHERE tenant_id = ?`, tenant)
	})
	return tenant
}

type toolGateway struct {
	mu     sync.Mutex
	bodies []map[string]any
	hits   atomic.Int32
}

func fakeToolGateway(t *testing.T, status int, response string) *toolGateway {
	t.Helper()
	g := &toolGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.bodies = append(g.bodies, body)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(UseGateway(srv.URL, "test-key"))
	return g
}

func toolOpts(tenant string) ToolChatOpts {
	return ToolChatOpts{
		Model: "openai/gpt-6-luna-pro",
		Messages: []ToolMessage{
			{Role: ToolRoleSystem, Content: "<role>static</role>"},
			{Role: ToolRoleUser, Content: "<demande>Inventaire</demande>"},
			{Role: ToolRoleAssistant, ToolCalls: []ToolCallWire{{ID: "call_1", Name: "list_pieces", Arguments: `{"progress_note":"x"}`}}},
			{Role: ToolRoleTool, ToolCallID: "call_1", Content: `{"status":"ok"}`},
		},
		Tools: []FunctionSpec{{Name: "list_pieces", Description: "Inventaire", Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"progress_note": map[string]any{"type": "string"}},
			"required":   []string{"progress_note"},
		}}},
		Purpose: "agent", TenantID: tenant, ActorID: "user-42", AgentDispatch: 7,
		InputManifestSHA256: strings.Repeat("a", 64),
	}
}

const toolCallCompletion = `{
  "id": "gen-1", "model": "openai/gpt-6-luna-pro-20261001",
  "choices": [{"finish_reason": "tool_calls", "message": {"role": "assistant", "content": null,
    "reasoning_details": [{"type": "reasoning.encrypted", "data": "opaque"}],
    "tool_calls": [
      {"id": "call_a", "type": "function", "function": {"name": "read_piece", "arguments": "{\"progress_note\":\"Je lis\"}"}},
      {"id": "call_b", "type": "function", "function": {"name": "list_pieces", "arguments": {"progress_note": "objet"}}}
    ]}}],
  "usage": {"prompt_tokens": 900, "completion_tokens": 50, "cost": 0.003,
    "prompt_tokens_details": {"cached_tokens": 512}, "completion_tokens_details": {"reasoning_tokens": 30}}
}`

// A completion made only of tool calls is a normal result, decoded call by
// call; the transcript goes out in the native shape; the row is reconciled
// with the returned model, the usage details and the actor.
func TestChatToolsAcceptsContentFreeToolCalls(t *testing.T) {
	tenant := toolTestDB(t)
	g := fakeToolGateway(t, http.StatusOK, toolCallCompletion)

	res, err := ChatTools(context.Background(), toolOpts(tenant))
	if err != nil {
		t.Fatalf("ChatTools: %v", err)
	}
	if res.Content != "" || len(res.ToolCalls) != 2 || res.FinishReason != "tool_calls" {
		t.Fatalf("result = %+v", res)
	}
	if res.ToolCalls[0].ID != "call_a" || res.ToolCalls[0].Arguments != `{"progress_note":"Je lis"}` {
		t.Fatalf("string arguments decoded as %+v", res.ToolCalls[0])
	}
	if res.ToolCalls[1].Arguments != `{"progress_note": "objet"}` {
		t.Fatalf("object arguments decoded as %q", res.ToolCalls[1].Arguments)
	}
	if res.Model != "openai/gpt-6-luna-pro-20261001" || len(res.ReasoningDetails) == 0 {
		t.Fatalf("returned model / reasoning continuation lost: %+v", res)
	}
	if res.Usage.CachedTokens != 512 || res.Usage.ReasoningTokens != 30 || res.UsageStatus != types.UsageStatusKnown {
		t.Fatalf("usage = %+v", res.Usage)
	}

	if g.hits.Load() != 1 {
		t.Fatalf("hits = %d", g.hits.Load())
	}
	body := g.bodies[0]
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}
	msgs := body["messages"].([]any)
	assistant := msgs[2].(map[string]any)
	if assistant["content"] != nil || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant tool-call message = %v", assistant)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", tool)
	}
	fn := body["tools"].([]any)[0].(map[string]any)
	if fn["type"] != "function" || fn["function"].(map[string]any)["name"] != "list_pieces" {
		t.Fatalf("tool spec = %v", fn)
	}
	if body["provider"].(map[string]any)["data_collection"] != "deny" {
		t.Fatal("the request must deny data collection")
	}

	var row types.LLMCall
	if err := DB.Where("id = ?", res.LLMCallID).Take(&row).Error; err != nil {
		t.Fatalf("llm_call: %v", err)
	}
	if row.State != types.LLMCallStateSucceeded || row.ReturnedModel != res.Model || row.ActorID != "user-42" ||
		row.AgentDispatch == nil || *row.AgentDispatch != 7 || row.InputManifestSHA256 != strings.Repeat("a", 64) ||
		row.UsageStatus != types.UsageStatusKnown || row.PromptTokens != 900 {
		t.Fatalf("llm_call not reconciled: %+v", row)
	}
	var details map[string]any
	_ = json.Unmarshal(row.UsageDetails, &details)
	if details["cached_tokens"] != float64(512) || details["reasoning_tokens"] != float64(30) {
		t.Fatalf("usage details = %v", details)
	}
	if row.TenantID != tenant || row.CostNanoUSD != 3_000_000 {
		t.Fatalf("cost record is not attributed: tenant=%s cost=%d nano-USD", row.TenantID, row.CostNanoUSD)
	}
}

// One attempt, never a hidden retry: a 503 returns at once, the row is
// closed failed with unknown usage, and nothing is billed.
func TestChatToolsMakesExactlyOneAttempt(t *testing.T) {
	tenant := toolTestDB(t)
	g := fakeToolGateway(t, http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`)

	res, err := ChatTools(context.Background(), toolOpts(tenant))
	var ge *GatewayError
	if !errors.As(err, &ge) || !ge.Retryable || ge.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a retryable 503", err)
	}
	if g.hits.Load() != 1 {
		t.Fatalf("hits = %d, want exactly one attempt", g.hits.Load())
	}
	var row types.LLMCall
	if err := DB.Where("id = ?", res.LLMCallID).Take(&row).Error; err != nil {
		t.Fatalf("llm_call: %v", err)
	}
	if row.State != types.LLMCallStateFailed || row.UsageStatus != types.UsageStatusUnknown {
		t.Fatalf("failed dispatch recorded as %s/%s", row.State, row.UsageStatus)
	}
	if row.CostNanoUSD != 0 {
		t.Fatal("a failed dispatch with unknown usage must not be billed")
	}
}

// Refusal and truncation stay explicit unusable outputs — recorded as spent.
func TestChatToolsRefusalAndTruncationAreUnusable(t *testing.T) {
	tenant := toolTestDB(t)
	cases := map[string]struct {
		response string
		want     error
	}{
		"refusal": {`{"id":"g","model":"m","choices":[{"finish_reason":"stop","message":{"content":"","refusal":"no"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.0001}}`, ErrRefused},
		"truncated": {`{"id":"g","model":"m","choices":[{"finish_reason":"length","message":{"content":"partiel"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0.0001}}`, ErrTruncated},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakeToolGateway(t, http.StatusOK, tc.response)
			res, err := ChatTools(context.Background(), toolOpts(tenant))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var row types.LLMCall
			DB.Where("id = ?", res.LLMCallID).Take(&row)
			if row.State != types.LLMCallStateSucceeded || row.UsageStatus != types.UsageStatusKnown {
				t.Fatalf("an unusable but billed call is recorded as %s/%s", row.State, row.UsageStatus)
			}
		})
	}
}

// Nothing is sent when the dispatch cannot be recorded.
func TestChatToolsRefusesToDispatchWithoutAccounting(t *testing.T) {
	tenant := toolTestDB(t)
	g := fakeToolGateway(t, http.StatusOK, toolCallCompletion)
	prev := DB
	DB = nil
	defer func() { DB = prev }()

	if _, err := ChatTools(context.Background(), toolOpts(tenant)); !errors.Is(err, ErrAccounting) {
		t.Fatalf("err = %v, want ErrAccounting", err)
	}
	if g.hits.Load() != 0 {
		t.Fatal("a dispatch left the process without its llm_calls row")
	}
}

// A dispatch reserved by the caller is reconciled in place: no second row.
func TestChatToolsReconcilesAReservedCall(t *testing.T) {
	tenant := toolTestDB(t)
	fakeToolGateway(t, http.StatusOK, toolCallCompletion)
	opts := toolOpts(tenant)
	row := NewLLMCallRecord(opts)
	if err := DB.Create(&row).Error; err != nil {
		t.Fatalf("reserve: %v", err)
	}
	opts.Reserved = &ReservedCall{ID: row.ID, ExecutionID: row.ExecutionID}

	res, err := ChatTools(context.Background(), opts)
	if err != nil || res.LLMCallID != row.ID || res.ExecutionID != row.ExecutionID {
		t.Fatalf("ChatTools = %+v, %v", res, err)
	}
	var n int64
	DB.Model(&types.LLMCall{}).Where("tenant_id = ?", tenant).Count(&n)
	if n != 1 {
		t.Fatalf("llm_calls rows = %d, want the reserved one only", n)
	}
	var stored types.LLMCall
	DB.Where("id = ?", row.ID).Take(&stored)
	if stored.State != types.LLMCallStateSucceeded {
		t.Fatalf("reserved row state = %s", stored.State)
	}
}

func TestValidateToolArguments(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"progress_note": map[string]any{"type": "string"},
			"kind":          map[string]any{"type": "string", "enum": []string{"a", "b"}},
		},
		"required": []string{"progress_note"},
	}
	cases := map[string]bool{
		`{"progress_note":"x"}`:                     true,
		`{"progress_note":"x","kind":"a"}`:          true,
		`{"kind":"a"}`:                              false, // a Go []string required list is enforced
		`{"progress_note":"x","kind":"z"}`:          false, // and so is a Go []string enum
		`{"progress_note":"x","tenant_id":"other"}`: false,
		`{"progress_note":1}`:                       false,
		`["progress_note"]`:                         false,
		`{"progress_note":"x"} trailing`:            false,
		``:                                          false,
	}
	for args, ok := range cases {
		if err := ValidateToolArguments(json.RawMessage(args), schema); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%t", args, err, ok)
		}
	}
}

func TestToolRequestHashChangesWithTheInput(t *testing.T) {
	opts := toolOpts("t")
	a, _ := ToolRequestSHA256(opts.Model, opts.Messages, opts.Tools)
	b, _ := ToolRequestSHA256(opts.Model, opts.Messages, opts.Tools)
	opts.Messages[1].Content += " "
	c, _ := ToolRequestSHA256(opts.Model, opts.Messages, opts.Tools)
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("hashes %s %s %s", a, b, c)
	}
}
