package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The agent runtime, tested deterministically: a REAL Postgres and the REAL
// runner (claims, leases, expansions, parking, cancellation), with a scripted
// fake OpenRouter and fake tools behind the Track 0 registry. What is under
// test is the loop's durability and its guarantees — the 40-dispatch ceiling,
// resume without re-paying, cancellation, parking, and that only a verified
// answer ever reaches the thread — not any model's judgement.

const fakeReturnedModel = "openai/gpt-6-luna-pro-20261001"

// ---------------------------------------------------------------------------
// Scripted gateway
// ---------------------------------------------------------------------------

type scriptedGateway struct {
	mu       sync.Mutex
	requests []map[string]any
	script   func(n int, body map[string]any) (int, string)
}

func newScriptedGateway(t *testing.T, script func(n int, body map[string]any) (int, string)) *scriptedGateway {
	t.Helper()
	g := &scriptedGateway{script: script}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.requests = append(g.requests, body)
		n := len(g.requests)
		g.mu.Unlock()
		status, resp := g.script(n, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(u.UseGateway(srv.URL, "test-key"))
	return g
}

func (g *scriptedGateway) hits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func (g *scriptedGateway) request(n int) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[n-1]
}

// requestText is the whole request body as text, for "does it contain"
// assertions.
func (g *scriptedGateway) requestText(n int) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(g.request(n))
	return b.String()
}

type fakeCall struct {
	ID, Name string
	Args     map[string]any
}

func toolCallsResponse(content string, calls ...fakeCall) string {
	wire := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		args, _ := json.Marshal(c.Args)
		wire = append(wire, map[string]any{
			"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": string(args)},
		})
	}
	var contentValue any
	if content != "" {
		contentValue = content
	}
	return completion("tool_calls", map[string]any{"role": "assistant", "content": contentValue, "tool_calls": wire})
}

func proseResponse(text string) string {
	return completion("stop", map[string]any{"role": "assistant", "content": text})
}

func completion(finish string, message map[string]any) string {
	raw, _ := json.Marshal(map[string]any{
		"id":    "gen-" + uuid.NewString()[:8],
		"model": fakeReturnedModel,
		"choices": []map[string]any{{
			"finish_reason": finish,
			"message":       message,
		}},
		"usage": map[string]any{
			"prompt_tokens": 1200, "completion_tokens": 80, "cost": 0.0042,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 600},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 40},
		},
	})
	return string(raw)
}

// ---------------------------------------------------------------------------
// Fake tools behind the Track 0 registry
// ---------------------------------------------------------------------------

type fakeTool struct {
	def  types.AgentToolDefinition
	exec func(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error)
}

func (f fakeTool) Definition() types.AgentToolDefinition { return f.def }
func (f fakeTool) Execute(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error) {
	return f.exec(ctx, x, args)
}

type fakeRegistry struct {
	order []string
	tools map[string]AgentTool
}

func (r *fakeRegistry) Register(tool AgentTool) error {
	def := tool.Definition()
	if err := ValidateToolDefinition(def); err != nil {
		return err
	}
	if r.tools == nil {
		r.tools = map[string]AgentTool{}
	}
	if _, dup := r.tools[def.Name]; dup {
		return fmt.Errorf("tool %s registered twice", def.Name)
	}
	r.tools[def.Name] = tool
	r.order = append(r.order, def.Name)
	return nil
}

func (r *fakeRegistry) Lookup(name string) (AgentTool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *fakeRegistry) Definitions() []types.AgentToolDefinition {
	out := make([]types.AgentToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name].Definition())
	}
	return out
}

func fakeSchema(props map[string]any, required ...string) map[string]any {
	p := map[string]any{types.ProgressNoteArg: types.ProgressNoteSchema()}
	for k, v := range props {
		p[k] = v
	}
	return map[string]any{
		"type": "object", "additionalProperties": false, "properties": p,
		"required": append([]string{types.ProgressNoteArg}, required...),
	}
}

// fakeListPiecesTool counts the scoped dossier's pieces through the call's
// tenant repository, and proposes one reading receipt for the text it
// returned — written in Persist, consumed only by a later accepted dispatch
// that was actually sent that text.
func fakeListPiecesTool() fakeTool {
	return fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolListPieces, Description: "Inventaire des pièces du dossier.",
			Parameters: fakeSchema(nil), Class: types.ToolClassDB, ReadOnly: true,
		},
		exec: func(ctx context.Context, x ToolExecution, _ json.RawMessage) (ToolOutcome, error) {
			var pieces []types.Piece
			if err := x.Repo(ctx).Pieces().Where("dossier_id = ?", x.Contract.Scope.DossierID).
				Order("ordinal").Find(&pieces).Error; err != nil {
				return ToolOutcome{}, err
			}
			data, _ := json.Marshal(map[string]any{"total": len(pieces)})
			total := len(pieces)
			receipt := types.AgentReadReceipt{
				ID: uuid.NewString(), RunID: x.RunID, PieceID: pieces[0].ID, SourceSHA256: "source",
				ExtractionVersion: "test", SegmentID: "p1", StartOffset: 0, EndOffset: 10,
				TextSHA256: "text", ToolCallID: x.CallID, RunStepID: x.StepID,
			}
			return ToolOutcome{
				Observation: types.ToolObservation{Status: types.ObservationOK, Data: data, ReadReceiptIDs: []string{receipt.ID}},
				Activity:    types.AgentActivityObject{Kind: types.ActivityObjectPieces, Label: "Inventaire du dossier"},
				Counts:      &types.AgentActivityCounts{Total: &total},
				Persist:     func(tx *u.TenantRepo) error { return tx.Create(&receipt) },
			}, nil
		},
	}
}

// blockingReadTool waits until released or until its context ends — what a
// long read looks like to the runner.
func blockingReadTool(release <-chan struct{}) fakeTool {
	return fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolReadPiece, Description: "Lecture d'une pièce.",
			Parameters: fakeSchema(nil), Class: types.ToolClassDB, ReadOnly: true,
		},
		exec: func(ctx context.Context, _ ToolExecution, _ json.RawMessage) (ToolOutcome, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return ToolOutcome{}, ctx.Err()
			}
			return ToolOutcome{Observation: types.ToolObservation{Status: types.ObservationOK}}, nil
		},
	}
}

// submitTool writes the answer revision the gate verifies, as Track B's
// submit_result does: in Persist, inside the runner's fenced commit.
func submitTool() fakeTool {
	return fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolSubmitResult, Description: "Soumet le résultat à la vérification finale.",
			Parameters: fakeSchema(map[string]any{
				"answer":     map[string]any{"type": "string"},
				"outcome":    map[string]any{"type": "string", "enum": []string{"completed", "partial"}},
				"limitation": map[string]any{"type": "string"},
			}, "answer"),
			Class: types.ToolClassDB, Exclusive: true,
		},
		exec: func(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error) {
			var a struct {
				Answer     string `json:"answer"`
				Outcome    string `json:"outcome"`
				Limitation string `json:"limitation"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return ToolOutcome{}, err
			}
			body := types.DocumentRevisionBody{SchemaVersion: types.DocumentSchemaVersion, Blocks: []types.DocumentBlock{
				{ID: "b1", Role: types.BlockParagraph, Text: a.Answer},
			}}
			if a.Limitation != "" {
				body.Blocks = append(body.Blocks, types.DocumentBlock{ID: "b2", Role: types.BlockLimitation, Text: a.Limitation})
			}
			sha, err := types.DocumentContentSHA256(types.AgentOutputAnswer, "", body)
			if err != nil {
				return ToolOutcome{}, err
			}
			raw, _ := json.Marshal(body)
			var existing int64
			x.Repo(ctx).AgentDocumentRevisions().Where("run_id = ? AND document_key = ?", x.RunID, "answer").Count(&existing)
			rev := types.AgentDocumentRevision{
				ID: uuid.NewString(), RunID: x.RunID, DocumentKey: "answer", Revision: int(existing) + 1,
				Kind: types.AgentOutputAnswer, ContentSHA256: sha, SchemaVersion: types.DocumentSchemaVersion,
				Body: types.JSONB(raw), CreatedByCallID: x.CallID,
			}
			data, _ := json.Marshal(map[string]any{"answer_revision_id": rev.ID, "outcome": a.Outcome})
			return ToolOutcome{
				Observation: types.ToolObservation{Status: types.ObservationOK, Data: data},
				Activity:    types.AgentActivityObject{Kind: types.ActivityObjectSubmission, Label: "Réponse soumise"},
				Persist:     func(tx *u.TenantRepo) error { return tx.Create(&rev) },
			}, nil
		},
	}
}

// askTool validates one material question and hands it to the runtime.
func askTool() fakeTool {
	return fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolAskLawyer, Description: "Pose une question matérielle à l'avocat.",
			Parameters: fakeSchema(map[string]any{"question": map[string]any{"type": "string"}}, "question"),
			Class:      types.ToolClassDB, Exclusive: true,
		},
		exec: func(_ context.Context, _ ToolExecution, args json.RawMessage) (ToolOutcome, error) {
			var a struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal(args, &a)
			data, _ := json.Marshal(map[string]any{"questions": []map[string]any{{
				"prompt": a.Question, "kind": types.QuestionKindSingle, "required": true,
				"options": []map[string]string{{"value": "oui", "label": "Oui"}, {"value": "non", "label": "Non"}},
			}}})
			return ToolOutcome{Observation: types.ToolObservation{Status: types.ObservationOK, Data: data}}, nil
		},
	}
}

// passingVerifier binds a passing report to the exact revision. blockFirst
// refuses the first n submissions, as a gate finding an unsupported legal
// reference would.
func passingVerifier(blockFirst int) func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
	var calls atomic.Int32
	return func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
		var rev types.AgentDocumentRevision
		if err := u.ForTenantID(ctx, in.TenantID).AgentDocumentRevisions().
			Where("run_id = ? AND id = ?", in.RunID, in.DocumentRevisionID).Take(&rev).Error; err != nil {
			return types.VerificationReport{}, err
		}
		report := types.VerificationReport{
			VerifierVersion: VerifierVersionFinal, DocumentRevisionID: rev.ID, ContentSHA256: rev.ContentSHA256,
			ScopeSHA256: "scope", EvidenceSHA256: "evidence", Result: types.VerificationPassed,
			DownloadAllowed: true, CheckedAt: time.Now().Unix(),
		}
		if int(calls.Add(1)) <= blockFirst {
			report.Result, report.DownloadAllowed, report.Blocking = types.VerificationBlocked, false, 1
			report.Issues = []types.VerificationIssue{{
				Code: types.IssueLegalNotRetrieved, Severity: types.SeverityBlocking, Check: types.CheckLegalCitations,
				BlockID: "b1", Correction: "Retirez la référence ou recherchez-la.",
			}}
		}
		return report, nil
	}
}

type fakePublisher struct{ calls atomic.Int32 }

func (p *fakePublisher) PublishResult(ctx context.Context, in PublicationInput) (PublicationResult, error) {
	p.calls.Add(1)
	if in.Answer.Verification.ID == "" || in.Answer.Revision.ID == "" {
		return PublicationResult{}, errors.New("publication without a verified answer")
	}
	// Freeze the answer on the message, as the real publisher does: delivery
	// streams PublishedAnswerText from exactly these rows.
	return PublicationResult{}, in.Fence(ctx, func(tx *u.TenantRepo) error {
		return tx.Messages().Where("id = ? AND run_id = ?", in.AssistantMessageID, in.RunID).Updates(map[string]any{
			"answer_revision_id": in.Answer.Revision.ID, "verification_id": in.Answer.Verification.ID, "agent_outcome": in.Outcome,
		}).Error
	})
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type agentHarness struct {
	f         *convFixture
	gw        *scriptedGateway
	registry  *fakeRegistry
	publisher *fakePublisher
}

var agentRunnerClockOnce sync.Once

func newAgentHarness(t *testing.T, script func(n int, body map[string]any) (int, string), tools ...fakeTool) *agentHarness {
	t.Helper()
	f := newFixture(t)
	// Registered after the fixture's cleanup, so it runs first: the agent's
	// own rows and the plan edges go before the runs they hang off.
	t.Cleanup(func() { cleanupAgentRows(f.firm) })

	h := &agentHarness{f: f, gw: newScriptedGateway(t, script), registry: &fakeRegistry{}, publisher: &fakePublisher{}}
	for _, tool := range tools {
		if err := h.registry.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.def.Name, err)
		}
	}
	prevTools, prevVerify, prevPublisher, prevArtifacts := AgentTools, VerifyOutput, AgentPublisher, AgentArtifacts
	AgentTools, VerifyOutput, AgentPublisher, AgentArtifacts = h.registry, passingVerifier(0), h.publisher, nil
	t.Cleanup(func() {
		AgentTools, VerifyOutput, AgentPublisher, AgentArtifacts = prevTools, prevVerify, prevPublisher, prevArtifacts
	})

	agentRunnerClockOnce.Do(func() {
		runner.PollInterval = 25 * time.Millisecond
		runner.BaseBackoff = 20 * time.Millisecond
		runner.MaxBackoff = 200 * time.Millisecond
		runner.DefaultLease = 60 * time.Second
		runner.HeartbeatInterval = 60 * time.Second
		runner.ReaperInterval = time.Hour
	})
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx, config.Config{StepConcurrencyOCR: 1, StepConcurrencyRender: 1, StepConcurrencyLLM: 2, StepConcurrencyDB: 4})
	t.Cleanup(func() {
		cancel()
		time.Sleep(60 * time.Millisecond) // let the pools see it before the fixture is torn down
	})
	return h
}

func cleanupAgentRows(firm string) {
	db := u.DB
	db.Exec(`DELETE FROM run_step_deps WHERE run_id IN (SELECT id FROM runs WHERE tenant_id = ?)`, firm)
	for _, table := range []string{
		"agent_turns", "agent_read_receipts", "agent_evidence_spans",
		"agent_document_revisions", "agent_verifications", "agent_web_sources",
	} {
		db.Exec(fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ?", table), firm)
	}
}

func (h *agentHarness) repo() *u.TenantRepo { return u.ForTenantID(context.Background(), h.f.firm) }

// post sends one turn to a fresh thread bound to the fixture's dossier.
func (h *agentHarness) post(t *testing.T, content string) (types.Conversation, types.MessageCreateResponse) {
	t.Helper()
	conv := mustCreate(t, h.f, types.ConversationCreateRequest{DossierID: &h.f.dossier})
	status, out := PostMessage(ctxFor(h.f.firm, h.f.user), conv.ID, types.MessageCreateRequest{Content: content})
	if status != http.StatusCreated {
		t.Fatalf("PostMessage: status %d body %v", status, out)
	}
	res, ok := out.(types.MessageCreateResponse)
	if !ok {
		t.Fatalf("PostMessage returned %T", out)
	}
	if res.Agent == nil || res.RunID == nil || *res.RunID != res.Agent.RunID {
		t.Fatalf("the turn did not enter the agent runtime: %+v", res)
	}
	return conv, res
}

func (h *agentHarness) waitRun(t *testing.T, runID string, cond func(types.Run) bool, what string) types.Run {
	t.Helper()
	var run types.Run
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.repo().Runs().Where("id = ?", runID).Take(&run).Error; err == nil && cond(run) {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (run=%s state=%s failure=%s)", what, runID, run.State, run.FailureCode)
	return run
}

func (h *agentHarness) waitTerminal(t *testing.T, runID string) types.Run {
	t.Helper()
	return h.waitRun(t, runID, func(r types.Run) bool { return types.TerminalRunState(r.State) }, "the run to end")
}

func (h *agentHarness) turn(t *testing.T, runID string) types.AgentTurn {
	t.Helper()
	var turn types.AgentTurn
	if err := h.repo().AgentTurns().Where("run_id = ?", runID).Take(&turn).Error; err != nil {
		t.Fatalf("agent turn: %v", err)
	}
	return turn
}

func (h *agentHarness) message(t *testing.T, id string) types.Message {
	t.Helper()
	var msg types.Message
	if err := h.repo().Messages().Where("id = ?", id).Take(&msg).Error; err != nil {
		t.Fatalf("message: %v", err)
	}
	return msg
}

func (h *agentHarness) events(t *testing.T, convID string) []types.RunEvent {
	t.Helper()
	events, err := readThreadEvents(h.repo(), convID, 0, 5000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return events
}

func payloadOf(ev types.RunEvent) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	return p
}

func note(text string) map[string]any { return map[string]any{types.ProgressNoteArg: text} }

func withNote(text string, extra map[string]any) map[string]any {
	args := note(text)
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// A turn runs a registered tool, observes it, submits, and the thread
// receives exactly the verified answer — streamed through message.delta and
// closed by message.complete. The model's private text never reaches it, and
// every dispatch is a durable, attributed llm_calls row.
func TestAgentLoopRunsAToolThenDeliversTheVerifiedAnswer(t *testing.T) {
	const private = "brouillon interne : le contrat est probablement nul"
	const answer = "Le dossier compte 3 pièces, toutes rattachées à la collection."
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		switch n {
		case 1:
			return 200, toolCallsResponse(private, fakeCall{ID: "call_list_1", Name: types.ToolListPieces,
				Args: note("Je recense les pièces du dossier.")})
		default:
			return 200, toolCallsResponse("", fakeCall{ID: "call_submit_1", Name: types.ToolSubmitResult,
				Args: withNote("Je soumets la réponse vérifiable.", map[string]any{"answer": answer})})
		}
	}, fakeListPiecesTool(), submitTool())

	conv, res := h.post(t, "Combien de pièces compte le dossier ?")
	if res.AssistantMessage.State != types.MessageStateStreaming || res.AssistantMessage.Content != "" {
		t.Fatalf("the assistant placeholder must be empty and streaming: %+v", res.AssistantMessage)
	}
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s, want succeeded", run.State, run.FailureCode)
	}
	if run.WorkflowType != types.WorkflowNomosAgent || run.ConversationID == nil || *run.ConversationID != conv.ID {
		t.Fatalf("run is not the thread's agent run: %+v", run)
	}

	turn := h.turn(t, run.ID)
	if turn.Outcome != types.AgentOutcomeCompleted || turn.Dispatches != 2 || turn.FinalVerificationID == nil {
		t.Fatalf("turn = outcome %q dispatches %d verification %v", turn.Outcome, turn.Dispatches, turn.FinalVerificationID)
	}
	msg := h.message(t, res.AssistantMessage.ID)
	if msg.State != types.MessageStateComplete || msg.Content != answer || msg.AgentOutcome != types.AgentOutcomeCompleted {
		t.Fatalf("assistant message = %q / %q / %q, want the verified answer", msg.State, msg.AgentOutcome, msg.Content)
	}
	if msg.AnswerRevisionID == nil || msg.VerificationID == nil || *msg.VerificationID != *turn.FinalVerificationID {
		t.Fatalf("the message does not point at its verified revision: %+v", msg)
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want 1", h.publisher.calls.Load())
	}

	// The transcript: native tool calls, then the observation paired by the
	// provider's call id, before the next dispatch.
	if h.gw.hits() != 2 {
		t.Fatalf("gateway hits = %d, want 2", h.gw.hits())
	}
	first := h.gw.request(1)
	msgs, _ := first["messages"].([]any)
	if len(msgs) < 3 {
		t.Fatalf("dispatch 1 sent %d messages", len(msgs))
	}
	system, _ := msgs[0].(map[string]any)
	if system["role"] != "system" || system["content"] != nomosAgentSystemPrompt {
		t.Fatal("the system prompt must be the static agent prompt")
	}
	if !strings.Contains(h.gw.requestText(1), "Combien de pièces compte le dossier") {
		t.Fatal("the instruction is not in dispatch 1's user context")
	}
	if tools, _ := first["tools"].([]any); len(tools) != 2 {
		t.Fatalf("dispatch 1 offered %d tools, want 2", len(tools))
	}
	second, _ := h.gw.request(2)["messages"].([]any)
	var sawCall, sawObservation bool
	for _, m := range second {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			if calls, _ := mm["tool_calls"].([]any); len(calls) == 1 {
				sawCall = true
			}
		}
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_list_1" &&
			strings.Contains(fmt.Sprint(mm["content"]), `"total":3`) {
			if !sawCall {
				t.Fatal("the observation precedes its tool call")
			}
			sawObservation = true
		}
	}
	if !sawObservation {
		t.Fatalf("dispatch 2 does not carry list_pieces' observation: %s", h.gw.requestText(2))
	}

	// The thread: activity rows per call, the deltas reassemble into the
	// stored text, and nothing the model wrote outside the verified revision
	// is in any event.
	var delivered strings.Builder
	running, done, complete := 0, 0, 0
	for _, ev := range h.events(t, conv.ID) {
		if strings.Contains(string(ev.Payload), "brouillon interne") {
			t.Fatalf("the model's private text leaked into %s", ev.Type)
		}
		p := payloadOf(ev)
		switch ev.Type {
		case types.EventAgentActivity:
			if p["call_id"] == "d1.c0" && p["status"] == types.ActivityRunning {
				running++
			}
			if p["call_id"] == "d1.c0" && p["status"] == types.ActivityOK {
				done++
				obj, _ := p["object"].(map[string]any)
				if obj["kind"] != types.ActivityObjectPieces || p["progress_note"] != "Je recense les pièces du dossier." {
					t.Fatalf("activity row does not describe the call: %v", p)
				}
			}
		case types.EventMessageDelta:
			delivered.WriteString(fmt.Sprint(p["delta"]))
		case types.EventMessageComplete:
			complete++
			if p["agent_outcome"] != types.AgentOutcomeCompleted || p["content"] != answer {
				t.Fatalf("message.complete = %v", p)
			}
		}
	}
	if running != 1 || done != 1 {
		t.Fatalf("activity rows for d1.c0: running=%d ok=%d, want 1 and 1", running, done)
	}
	if delivered.String() != answer || complete != 1 {
		t.Fatalf("deltas %q (complete=%d), want the verified answer once", delivered.String(), complete)
	}

	// Every dispatch is attributed and reconciled.
	var calls []types.LLMCall
	h.repo().LLMCalls().Where("run_id = ?", run.ID).Order("agent_dispatch").Find(&calls)
	if len(calls) != 2 {
		t.Fatalf("llm_calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.AgentDispatch == nil || *c.AgentDispatch != i+1 || c.ActorID != h.f.user ||
			c.InputManifestSHA256 == "" || c.ReturnedModel != fakeReturnedModel ||
			c.State != types.LLMCallStateSucceeded || c.UsageStatus != types.UsageStatusKnown ||
			c.Model != turn.ControllerModel || c.Purpose != agentPurpose {
			t.Fatalf("llm_call %d is not a reconciled, attributed dispatch: %+v", i+1, c)
		}
	}
	// The text list_pieces returned was in dispatch 2's input, and dispatch 2
	// was accepted: its receipt is consumed by dispatch 2, not before.
	var receipt types.AgentReadReceipt
	if err := h.repo().AgentReadReceipts().Where("run_id = ?", run.ID).Take(&receipt).Error; err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if receipt.ConsumedDispatch == nil || *receipt.ConsumedDispatch != 2 {
		t.Fatalf("receipt consumed by %v, want dispatch 2", receipt.ConsumedDispatch)
	}
}

// smallTool answers with a few dozen characters, as write_document does
// with the id of the revision it created.
func smallTool(name string) fakeTool {
	return fakeTool{
		def: types.AgentToolDefinition{
			Name: name, Description: "Petit résultat.", Parameters: fakeSchema(nil), Class: types.ToolClassDB, ReadOnly: true,
		},
		exec: func(context.Context, ToolExecution, json.RawMessage) (ToolOutcome, error) {
			data, _ := json.Marshal(map[string]any{"revision_id": "rev_small"})
			return ToolOutcome{
				Observation: types.ToolObservation{Status: types.ObservationOK, Data: data},
				Activity:    types.AgentActivityObject{Kind: types.ActivityObjectDocument, Label: "Petit résultat"},
			}, nil
		},
	}
}

// paddedListPiecesTool is fakeListPiecesTool with a bulky observation: one
// worth replacing by an eviction stub.
func paddedListPiecesTool() fakeTool {
	tool := fakeListPiecesTool()
	exec := tool.exec
	tool.exec = func(ctx context.Context, x ToolExecution, raw json.RawMessage) (ToolOutcome, error) {
		out, err := exec(ctx, x, raw)
		if err == nil {
			out.Observation.Data, _ = json.Marshal(map[string]any{"total": 3, "text": strings.Repeat("Texte de pièce. ", 300)})
		}
		return out, err
	}
	return tool
}

// Text evicted from the transcript is neither sent nor counted as read: the
// controller sees an explicit stub with the evidence ids, and the receipt
// stays unconsumed.
func TestAgentEvictedTextIsNeitherSentNorCountedAsRead(t *testing.T) {
	prev := agentContextChars
	agentContextChars = 1 // everything that can be evicted is
	t.Cleanup(func() { agentContextChars = prev })

	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_l", Name: types.ToolListPieces, Args: note("Inventaire.")})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Lecture incomplète signalée."})})
	}, paddedListPiecesTool(), submitTool())

	_, res := h.post(t, "Inventaire ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	second := h.gw.requestText(2)
	if strings.Contains(second, `\"total\":3`) || !strings.Contains(second, `\"evicted\":true`) {
		t.Fatalf("dispatch 2 must carry the stub, not the observation: %s", second)
	}
	var receipt types.AgentReadReceipt
	if err := h.repo().AgentReadReceipts().Where("run_id = ?", run.ID).Take(&receipt).Error; err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if receipt.ConsumedDispatch != nil {
		t.Fatalf("evicted text counted as read by dispatch %d", *receipt.ConsumedDispatch)
	}
	var rec agentDecisionRecord
	var step types.RunStep
	h.repo().RunSteps().Where("run_id = ? AND step_key = ?", run.ID, agentDecisionKey(2)).Take(&step)
	if !decodeRecord(step.Output, &rec) || len(rec.Manifest.Evicted) != 1 || rec.Manifest.Evicted[0] != "d1.c0" {
		t.Fatalf("dispatch 2's manifest does not record the eviction: %+v", rec.Manifest)
	}
}

// Eviction only replaces an observation its stub actually shrinks. A small
// result — a revision id, a verdict — costs less whole than as a stub, so it
// stays even when the transcript is over budget; otherwise the controller
// loses the id of the revision it just wrote and the context grows anyway.
func TestAgentEvictionKeepsObservationsSmallerThanTheirStub(t *testing.T) {
	prev := agentContextChars
	agentContextChars = 1 // over budget whatever is evicted
	t.Cleanup(func() { agentContextChars = prev })

	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		switch n {
		case 1:
			return 200, toolCallsResponse("", fakeCall{ID: "call_big", Name: types.ToolListPieces, Args: note("Lecture.")})
		case 2:
			return 200, toolCallsResponse("", fakeCall{ID: "call_small", Name: types.ToolReadPiece, Args: note("Inventaire.")})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Réponse."})})
	}, paddedListPiecesTool(), smallTool(types.ToolReadPiece), submitTool())

	_, res := h.post(t, "Inventaire ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	third := h.gw.requestText(3)
	if strings.Contains(third, "Texte de pièce.") {
		t.Fatalf("dispatch 3 still carries the bulky observation: %s", third)
	}
	if !strings.Contains(third, `\"revision_id\":\"rev_small\"`) {
		t.Fatalf("dispatch 3 lost the small observation to a larger stub: %s", third)
	}
	var rec agentDecisionRecord
	var step types.RunStep
	h.repo().RunSteps().Where("run_id = ? AND step_key = ?", run.ID, agentDecisionKey(3)).Take(&step)
	if !decodeRecord(step.Output, &rec) || len(rec.Manifest.Evicted) != 1 || rec.Manifest.Evicted[0] != "d1.c0" {
		t.Fatalf("dispatch 3 should evict only the bulky observation: %+v", rec.Manifest)
	}
}

// Free prose is never an answer: it becomes a submission_required correction
// that costs a dispatch, and only the submitted, verified text is delivered.
func TestAgentFreeProseIsNeverDelivered(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, proseResponse("Voici directement la réponse : le licenciement est nul.")
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Réponse vérifiée."})})
	}, submitTool())

	conv, res := h.post(t, "Le licenciement est-il valable ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	if msg := h.message(t, res.AssistantMessage.ID); msg.Content != "Réponse vérifiée." {
		t.Fatalf("delivered %q, want only the verified submission", msg.Content)
	}
	if !strings.Contains(h.gw.requestText(2), "<correction>") {
		t.Fatal("dispatch 2 does not tell the controller its prose was not delivered")
	}
	for _, ev := range h.events(t, conv.ID) {
		if strings.Contains(string(ev.Payload), "licenciement est nul") {
			t.Fatalf("unverified prose reached the thread in %s", ev.Type)
		}
	}
	if turn := h.turn(t, run.ID); turn.Dispatches != 2 {
		t.Fatalf("dispatches = %d, want 2 (the correction consumes one)", turn.Dispatches)
	}
}

// A dossier the composer opened around dropped files is named by the turn
// that sent them in agent mode too: the agent intake must not skip the
// naming the routed path does.
func TestAgentTurnNamesItsComposerDossier(t *testing.T) {
	h := newAgentHarness(t, func(_ int, body map[string]any) (int, string) {
		if strings.Contains(fmt.Sprint(body["messages"]), "<fichiers>") {
			return 200, proseResponse("Vasseur c/ NORDLOG — licenciement")
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Réponse vérifiée."})})
	}, submitTool())
	repo := h.repo()
	if err := repo.Dossiers().Where("id = ?", h.f.dossier).
		Updates(map[string]any{"title": "Nouveau dossier", "title_pending": true}).Error; err != nil {
		t.Fatal(err)
	}

	conv, res := h.post(t, "Établis la chronologie du dossier.")
	h.waitTerminal(t, res.Agent.RunID)
	var d types.Dossier
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		if err := repo.Dossiers().Where("id = ?", h.f.dossier).Take(&d).Error; err != nil {
			t.Fatal(err)
		}
		if !d.TitlePending || time.Now().After(deadline) {
			break
		}
	}
	if d.TitlePending || d.Title != "Vasseur c/ NORDLOG — licenciement" {
		t.Fatalf("the dossier was not named: title=%q pending=%t", d.Title, d.TitlePending)
	}
	titled := 0
	for _, ev := range h.events(t, conv.ID) {
		if ev.Type == EventDossierTitled {
			titled++
		}
	}
	if titled != 1 {
		t.Fatalf("want one %s event on the thread, got %d", EventDossierTitled, titled)
	}
}

// The agent intake binds a requested dossier inside its own transaction, so
// the naming must follow the dossier the turn was accepted on: a message that
// binds the thread, and a thread created with its first message, both name
// the composer's placeholder.
func TestAgentIntakeNamesTheDossierItBinds(t *testing.T) {
	h := newAgentHarness(t, func(_ int, body map[string]any) (int, string) {
		if strings.Contains(fmt.Sprint(body["messages"]), "<fichiers>") {
			return 200, proseResponse("Vasseur c/ NORDLOG — licenciement")
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Réponse vérifiée."})})
	}, submitTool())
	repo := h.repo()
	entries := map[string]func(t *testing.T) string{
		"message binds the thread": func(t *testing.T) string {
			conv := mustCreate(t, h.f, types.ConversationCreateRequest{})
			status, out := PostMessage(ctxFor(h.f.firm, h.f.user), conv.ID,
				types.MessageCreateRequest{Content: "Établis la chronologie du dossier.", DossierID: &h.f.dossier})
			res, ok := out.(types.MessageCreateResponse)
			if status != http.StatusCreated || !ok || res.Agent == nil {
				t.Fatalf("PostMessage: status %d body %v", status, out)
			}
			return res.Agent.RunID
		},
		"thread created with its first message": func(t *testing.T) string {
			status, out := CreateConversation(ctxFor(h.f.firm, h.f.user), types.ConversationCreateRequest{
				Message: &types.MessageCreateRequest{Content: "Établis la chronologie du dossier.", DossierID: &h.f.dossier},
			})
			detail, ok := out.(types.ConversationDetail)
			if status != http.StatusCreated || !ok || len(detail.Runs) != 1 {
				t.Fatalf("CreateConversation: status %d body %v", status, out)
			}
			return detail.Runs[0].ID
		},
	}
	for name, start := range entries {
		t.Run(name, func(t *testing.T) {
			if err := repo.Dossiers().Where("id = ?", h.f.dossier).
				Updates(map[string]any{"title": "Nouveau dossier", "title_pending": true}).Error; err != nil {
				t.Fatal(err)
			}
			h.waitTerminal(t, start(t))
			var d types.Dossier
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(25 * time.Millisecond) {
				if err := repo.Dossiers().Where("id = ?", h.f.dossier).Take(&d).Error; err != nil {
					t.Fatal(err)
				}
				if !d.TitlePending || time.Now().After(deadline) {
					break
				}
			}
			if d.TitlePending || d.Title != "Vasseur c/ NORDLOG — licenciement" {
				t.Fatalf("the dossier was not named: title=%q pending=%t", d.Title, d.TitlePending)
			}
		})
	}
}

// A refused submission goes back to the agent as submit_result's observation;
// only the submission that passes the gate is published and delivered.
func TestAgentGateRefusalGoesBackToTheAgent(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		answer := "Première version, avec une référence inventée."
		if n > 1 {
			answer = "Version corrigée, sans référence non vérifiée."
		}
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": answer})})
	}, submitTool())
	VerifyOutput = passingVerifier(1)

	_, res := h.post(t, "Quel est le délai de préavis applicable ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != 2 {
		t.Fatalf("run = %s/%s after %d dispatches", run.State, run.FailureCode, h.gw.hits())
	}
	if !strings.Contains(h.gw.requestText(2), types.ToolCodeVerificationRefused) {
		t.Fatal("dispatch 2 does not carry the gate's refusal")
	}
	if msg := h.message(t, res.AssistantMessage.ID); msg.Content != "Version corrigée, sans référence non vérifiée." {
		t.Fatalf("delivered %q, want the corrected, verified version", msg.Content)
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want only for the passing submission", h.publisher.calls.Load())
	}
}

// The lawyer's own words decide whether the work spans the whole dossier —
// at intake, by the server, never by the model.
func TestAgentIntakeMarksExhaustiveRequests(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Réponse."})})
	}, submitTool())
	for instruction, want := range map[string]bool{
		"Établis la chronologie du dossier, pièce par pièce, avec la source de chaque date.": true,
		"Fais une synthèse exhaustive des griefs.":                                           true,
		"Vérifie toutes les pièces relatives au salaire.":                                    true,
		"Combien de pièces compte le dossier ?":                                              false,
		"Que dit la lettre de licenciement sur le préavis ?":                                 false,
	} {
		_, res := h.post(t, instruction)
		h.waitTerminal(t, res.Agent.RunID)
		turn := h.turn(t, res.Agent.RunID)
		if got := turnContract(&turn).Exhaustive; got != want {
			t.Fatalf("%q: exhaustive = %t, want %t", instruction, got, want)
		}
	}
}

// An exhaustive request read in part can be delivered — as partial. A
// submission claiming completion is refused back to the agent, which then
// resubmits it as partial; it never reaches publication as complete.
func TestAgentGateRefusesCompletionOverIncompleteCoverage(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		args := map[string]any{"answer": "Chronologie établie sur les pièces lues.", "outcome": types.AgentOutcomeCompleted}
		if n > 1 {
			args["outcome"], args["limitation"] = types.AgentOutcomePartial, "Les pièces 4 à 18 n'ont pas été lues."
		}
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la chronologie.", args)})
	}, submitTool())
	pass := passingVerifier(0)
	VerifyOutput = func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
		r, err := pass(ctx, in)
		// Three pièces of eighteen read, the gap labelled: a warning.
		r.Coverage = &types.CoverageReport{Exhaustive: true, Complete: false, TotalPieces: 18, FullyRead: 3}
		r.Result, r.Warnings = types.VerificationWarnings, 1
		r.Issues = []types.VerificationIssue{{Code: types.IssueCoverageIncomplete, Severity: types.SeverityWarning, Check: types.CheckCoverage}}
		return r, err
	}

	_, res := h.post(t, "Établis la chronologie du dossier, pièce par pièce.")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != 2 {
		t.Fatalf("run = %s/%s after %d dispatches", run.State, run.FailureCode, h.gw.hits())
	}
	if text := h.gw.requestText(2); !strings.Contains(text, types.ToolCodeVerificationRefused) || !strings.Contains(text, "partial") {
		t.Fatal("dispatch 2 does not carry the refusal of a complete outcome")
	}
	if turn := h.turn(t, run.ID); turn.Outcome != types.AgentOutcomePartial {
		t.Fatalf("turn outcome = %q, want partial", turn.Outcome)
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want only for the partial submission", h.publisher.calls.Load())
	}
}

// The ceiling holds through transport failures: a failed dispatch keeps its
// slot, the retry reserves the next one, and once the ceiling is reached the
// turn ends with the server's explanation — no further model call, not even
// to describe the failure.
func TestAgentDispatchCeilingHoldsThroughRetries(t *testing.T) {
	prev := agentMaxDispatches
	agentMaxDispatches = 4
	t.Cleanup(func() { agentMaxDispatches = prev })

	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 2 {
			return http.StatusServiceUnavailable, `{"error":{"message":"upstream overloaded"}}`
		}
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_%d", n), Name: types.ToolListPieces,
			Args: note("Je relis l'inventaire.")})
	}, fakeListPiecesTool(), submitTool())

	conv, res := h.post(t, "Établis la chronologie du dossier.")
	if res.Agent.MaxDispatches != 4 {
		t.Fatalf("max dispatches = %d, want the lowered ceiling", res.Agent.MaxDispatches)
	}
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateFailed || run.FailureCode != types.AgentOutcomeTurnLimit {
		t.Fatalf("run = %s/%s, want failed/agent_turn_limit", run.State, run.FailureCode)
	}
	time.Sleep(300 * time.Millisecond) // give a runaway loop the chance to show itself
	if h.gw.hits() != 4 {
		t.Fatalf("gateway hits = %d, want exactly the ceiling of 4", h.gw.hits())
	}
	turn := h.turn(t, run.ID)
	if turn.Dispatches != 4 || turn.Outcome != types.AgentOutcomeTurnLimit {
		t.Fatalf("turn = dispatches %d outcome %q", turn.Dispatches, turn.Outcome)
	}
	var calls []types.LLMCall
	h.repo().LLMCalls().Where("run_id = ?", run.ID).Order("agent_dispatch").Find(&calls)
	if len(calls) != 4 {
		t.Fatalf("llm_calls = %d, want 4 (the failed dispatch included)", len(calls))
	}
	for i, c := range calls {
		if c.AgentDispatch == nil || *c.AgentDispatch != i+1 {
			t.Fatalf("dispatch numbers are not 1..4: %+v", calls)
		}
	}
	if calls[1].State != types.LLMCallStateFailed {
		t.Fatalf("the 503 dispatch is %s, want failed", calls[1].State)
	}
	msg := h.message(t, res.AssistantMessage.ID)
	if msg.State != types.MessageStateComplete || msg.AgentOutcome != types.AgentOutcomeTurnLimit ||
		!strings.Contains(msg.Content, "limite de travail") || strings.Contains(msg.Content, "renvoyez") {
		t.Fatalf("explanation = %q / %q / %q", msg.State, msg.AgentOutcome, msg.Content)
	}
	var completes int
	for _, ev := range h.events(t, conv.ID) {
		if ev.Type == types.EventMessageComplete {
			completes++
		}
	}
	if completes != 1 {
		t.Fatalf("message.complete events = %d, want 1", completes)
	}
}

// A process death after the response is recorded and before it is acted on
// resumes from the record: the dispatch is not paid twice and its number is
// not consumed again.
func TestAgentResumesARecordedDispatchWithoutPayingAgain(t *testing.T) {
	var crashed atomic.Bool
	prev := agentDecisionRecorded
	agentDecisionRecorded = func(_ string, dispatch int) error {
		if dispatch == 1 && crashed.CompareAndSwap(false, true) {
			return runner.Retry("simulated_crash", "the process died after recording the response")
		}
		return nil
	}
	t.Cleanup(func() { agentDecisionRecorded = prev })

	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_l", Name: types.ToolListPieces, Args: note("Inventaire.")})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Trois pièces."})})
	}, fakeListPiecesTool(), submitTool())

	_, res := h.post(t, "Inventaire du dossier ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || !crashed.Load() {
		t.Fatalf("run = %s/%s crashed=%t", run.State, run.FailureCode, crashed.Load())
	}
	if h.gw.hits() != 2 {
		t.Fatalf("gateway hits = %d, want 2: the recorded dispatch was paid again", h.gw.hits())
	}
	if turn := h.turn(t, run.ID); turn.Dispatches != 2 {
		t.Fatalf("dispatches = %d, want 2", turn.Dispatches)
	}
	var step types.RunStep
	h.repo().RunSteps().Where("run_id = ? AND step_key = ?", run.ID, agentDecisionKey(1)).Take(&step)
	if step.AttemptCount != 2 || step.State != types.StepStateSucceeded {
		t.Fatalf("decision 1 = %s after %d attempts, want succeeded after 2", step.State, step.AttemptCount)
	}
	var first int64
	h.repo().LLMCalls().Where("run_id = ? AND agent_dispatch = 1", run.ID).Count(&first)
	if first != 1 {
		t.Fatalf("llm_calls for dispatch 1 = %d, want 1", first)
	}
	if msg := h.message(t, res.AssistantMessage.ID); msg.Content != "Trois pièces." {
		t.Fatalf("delivered %q", msg.Content)
	}
}

// Cancellation stops future work: the running tool's result is not accepted,
// no further dispatch happens, and the turn closes with the server's words.
func TestAgentCancellationStopsTheLoop(t *testing.T) {
	release := make(chan struct{})
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_r", Name: types.ToolReadPiece, Args: note("Je lis la pièce 1.")})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Ne doit jamais arriver."})})
	}, blockingReadTool(release), submitTool())

	conv, res := h.post(t, "Lis la pièce 1.")
	h.waitRun(t, res.Agent.RunID, func(types.Run) bool {
		var s types.RunStep
		return h.repo().RunSteps().Where("run_id = ? AND step_key = ?", res.Agent.RunID, agentToolKey(1, 0)).
			Take(&s).Error == nil && s.State == types.StepStateRunning
	}, "the tool to start")

	status, out := CancelRun(ctxFor(h.f.firm, h.f.user), res.Agent.RunID)
	if status != http.StatusOK {
		t.Fatalf("CancelRun: %d %v", status, out)
	}
	close(release)

	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateCancelled {
		t.Fatalf("run = %s, want cancelled", run.State)
	}
	h.waitRun(t, run.ID, func(types.Run) bool {
		var s types.RunStep
		return h.repo().RunSteps().Where("run_id = ? AND step_key = ?", run.ID, agentToolKey(1, 0)).
			Take(&s).Error == nil && types.TerminalStepState(s.State)
	}, "the tool step to settle")
	time.Sleep(200 * time.Millisecond)

	if h.gw.hits() != 1 {
		t.Fatalf("gateway hits = %d after cancellation, want 1", h.gw.hits())
	}
	if turn := h.turn(t, run.ID); turn.Outcome != types.AgentOutcomeCancelled || turn.FinishedAt == nil {
		t.Fatalf("turn outcome = %q", turn.Outcome)
	}
	msg := h.message(t, res.AssistantMessage.ID)
	if msg.State != types.MessageStateComplete || !strings.Contains(msg.Content, "Demande annulée") {
		t.Fatalf("assistant message = %q / %q", msg.State, msg.Content)
	}
	for _, ev := range h.events(t, conv.ID) {
		p := payloadOf(ev)
		if ev.Type == types.EventAgentActivity && p["call_id"] == "d1.c0" &&
			(p["status"] == types.ActivityOK || p["status"] == types.ActivityEmpty) {
			t.Fatalf("the cancelled tool's result was accepted: %v", p)
		}
		if strings.Contains(string(ev.Payload), "Ne doit jamais arriver") {
			t.Fatal("work continued after cancellation")
		}
	}
}

// ask_lawyer parks on the existing ask_user step; the run waits without a
// dispatch, answers are validated, and the next decision observes them. The
// dispatch count is never reset by a clarification.
func TestAgentAskLawyerParksAndResumes(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_q", Name: types.ToolAskLawyer,
				Args: withNote("Je vous demande une précision indispensable.",
					map[string]any{"question": "La salariée a-t-elle été convoquée à un entretien préalable ?"})})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Compte tenu de l'entretien préalable, la procédure est régulière sur ce point."})})
	}, askTool(), submitTool())

	conv, res := h.post(t, "La procédure de licenciement est-elle régulière ?")
	runID := res.Agent.RunID
	h.waitRun(t, runID, func(r types.Run) bool { return r.State == types.RunStateWaitingInput }, "the question to park")
	time.Sleep(200 * time.Millisecond)
	if h.gw.hits() != 1 {
		t.Fatalf("gateway hits while parked = %d, want 1", h.gw.hits())
	}
	var q types.RunQuestion
	if err := h.repo().RunQuestions().Where("run_id = ?", runID).Take(&q).Error; err != nil {
		t.Fatalf("question row: %v", err)
	}
	if q.QuestionKey != agentQuestionKey(1, 1) || !q.Required || q.State != types.QuestionStatePending {
		t.Fatalf("question = %+v", q)
	}
	if turn := h.turn(t, runID); turn.ParkedAt == nil {
		t.Fatal("the turn is not marked parked")
	}

	// One active turn per thread: a new instruction is refused, the parked
	// question is answered instead.
	status, out := PostMessage(ctxFor(h.f.firm, h.f.user), conv.ID, types.MessageCreateRequest{Content: "Autre chose ?"})
	if body, _ := out.(map[string]any); status != http.StatusConflict || body["code"] != IntakeActiveTurn || body["run_id"] != runID {
		t.Fatalf("second turn: %d %v, want 409 active_turn", status, out)
	}

	answer := func(values ...string) (int, any) {
		return AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
			{QuestionKey: q.QuestionKey, Values: values},
		}})
	}
	if status, out := answer("peut-être"); status != http.StatusUnprocessableEntity {
		t.Fatalf("an option that was not offered: %d %v, want 422", status, out)
	}
	if status, out := AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
		{QuestionKey: "agent:9:q9", Values: []string{"oui"}},
	}}); status != http.StatusNotFound {
		t.Fatalf("an unknown question: %d %v, want 404", status, out)
	}
	if status, out := answer("oui"); status != http.StatusOK {
		t.Fatalf("answer: %d %v", status, out)
	}

	run := h.waitTerminal(t, runID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	if status, _ := answer("oui"); status != http.StatusBadRequest && status != http.StatusOK {
		t.Fatalf("replaying the same answer on a finished run: %d", status)
	}
	if h.gw.hits() != 2 {
		t.Fatalf("gateway hits = %d, want 2", h.gw.hits())
	}
	second := h.gw.requestText(2)
	if !strings.Contains(second, `\"state\":\"answered\"`) || !strings.Contains(second, `\"values\":[\"oui\"]`) {
		t.Fatalf("dispatch 2 does not observe the lawyer's answer: %s", second)
	}
	turn := h.turn(t, runID)
	if turn.Dispatches != 2 || turn.ParkedAt != nil {
		t.Fatalf("turn = dispatches %d parked %v, want 2 and not parked", turn.Dispatches, turn.ParkedAt)
	}
	if msg := h.message(t, res.AssistantMessage.ID); !strings.HasPrefix(msg.Content, "Compte tenu de l'entretien") {
		t.Fatalf("delivered %q", msg.Content)
	}
}

// A changed answer to a settled question is refused; the same answer
// replayed is idempotent.
func TestAgentAnswersAreValidatedAgainstTheirQuestion(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: "call_q", Name: types.ToolAskLawyer,
			Args: withNote("Question.", map[string]any{"question": "Le salarié avait-il plus de deux ans d'ancienneté ?"})})
	}, askTool(), submitTool())
	_, res := h.post(t, "Calcule l'indemnité de licenciement.")
	runID := res.Agent.RunID
	h.waitRun(t, runID, func(r types.Run) bool { return r.State == types.RunStateWaitingInput }, "the question to park")

	key := agentQuestionKey(1, 1)
	if status, out := AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
		{QuestionKey: key, Skipped: true},
	}}); status != http.StatusOK {
		t.Fatalf("skip: %d %v", status, out)
	}
	if status, out := AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
		{QuestionKey: key, Skipped: true},
	}}); status != http.StatusOK {
		t.Fatalf("the same skip replayed: %d %v, want 200", status, out)
	}
	if status, out := AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
		{QuestionKey: key, Values: []string{"oui"}},
	}}); status != http.StatusConflict {
		t.Fatalf("a changed answer to a settled question: %d %v, want 409", status, out)
	}
	var q types.RunQuestion
	h.repo().RunQuestions().Where("run_id = ? AND question_key = ?", runID, key).Take(&q)
	if q.State != types.QuestionStateSkipped {
		t.Fatalf("question state = %s, want the recorded skip", q.State)
	}
}

// ---------------------------------------------------------------------------
// Pure functions
// ---------------------------------------------------------------------------

func TestAgentCallValidation(t *testing.T) {
	byName := map[string]types.AgentToolDefinition{}
	for _, tool := range []fakeTool{fakeListPiecesTool(), submitTool(), askTool()} {
		byName[tool.def.Name] = tool.def
	}
	args := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	cases := []struct {
		name      string
		calls     []u.ToolCallWire
		rejection string
		prefailed map[int]string
	}{
		{"prose", nil, types.AgentOutcomeSubmissionRequired, nil},
		{"unknown tool", []u.ToolCallWire{{ID: "a", Name: "run_workflow", Arguments: args(note("x"))}},
			types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeUnknownTool}},
		{"extra field", []u.ToolCallWire{{ID: "a", Name: types.ToolListPieces, Arguments: args(withNote("x", map[string]any{"tenant_id": "other"}))}},
			types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeInvalidArguments}},
		{"broken json", []u.ToolCallWire{{ID: "a", Name: types.ToolListPieces, Arguments: `{"progress_note": `}},
			types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeInvalidArguments}},
		{"missing note", []u.ToolCallWire{{ID: "a", Name: types.ToolListPieces, Arguments: `{}`}},
			types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeInvalidArguments}},
		{"long note", []u.ToolCallWire{{ID: "a", Name: types.ToolListPieces, Arguments: args(note(strings.Repeat("é", 141)))}},
			types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeInvalidArguments}},
		{"exclusive mixed", []u.ToolCallWire{
			{ID: "a", Name: types.ToolListPieces, Arguments: args(note("x"))},
			{ID: "b", Name: types.ToolSubmitResult, Arguments: args(withNote("y", map[string]any{"answer": "z"}))},
		}, types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeExclusiveCall, 1: types.ToolCodeExclusiveCall}},
		{"too many", []u.ToolCallWire{
			{ID: "a", Name: types.ToolListPieces, Arguments: args(note("1"))},
			{ID: "b", Name: types.ToolListPieces, Arguments: args(note("2"))},
			{ID: "c", Name: types.ToolListPieces, Arguments: args(note("3"))},
			{ID: "d", Name: types.ToolListPieces, Arguments: args(note("4"))},
			{ID: "e", Name: types.ToolListPieces, Arguments: args(note("5"))},
		}, types.AgentOutcomeInvalidToolCall, map[int]string{0: types.ToolCodeTooManyCalls, 4: types.ToolCodeTooManyCalls}},
		{"mixed valid and invalid", []u.ToolCallWire{
			{ID: "a", Name: types.ToolListPieces, Arguments: args(note("ok"))},
			{ID: "a", Name: "shell", Arguments: args(note("no"))},
		}, "", map[int]string{1: types.ToolCodeUnknownTool}},
		{"valid", []u.ToolCallWire{{ID: "a", Name: types.ToolListPieces, Arguments: args(note("ok"))}}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := agentDecisionRecord{RawCalls: tc.calls}
			rec.Dispatch = 3
			agentValidateCalls(&rec, byName)
			if rec.Rejection != tc.rejection {
				t.Fatalf("rejection = %q, want %q", rec.Rejection, tc.rejection)
			}
			for i, code := range tc.prefailed {
				if got := rec.Prefailed[i].Code; got != code {
					t.Fatalf("call %d code = %q, want %q", i, got, code)
				}
			}
			if tc.prefailed == nil && len(rec.Prefailed) != 0 {
				t.Fatalf("unexpected refusals: %v", rec.Prefailed)
			}
			seen := map[string]bool{}
			for i, c := range rec.Calls {
				if c.ID != types.AgentCallID(3, i) || c.ProviderID == "" || seen[c.ProviderID] {
					t.Fatalf("call %d identity = %+v", i, c)
				}
				seen[c.ProviderID] = true
				if !json.Valid(c.Arguments) {
					t.Fatalf("call %d arguments are not storable JSON", i)
				}
			}
		})
	}
}

// Calls running in parallel cannot spend past a separate budget together:
// the batch is metered at acceptance.
func TestAgentBudgetsAreMeteredAtAcceptance(t *testing.T) {
	webDef := types.AgentToolDefinition{
		Name: types.ToolSearchWeb, Parameters: fakeSchema(map[string]any{"query": map[string]any{"type": "string"}}, "query"),
		Class: types.ToolClassExternal, ReadOnly: true,
	}
	byName := map[string]types.AgentToolDefinition{types.ToolSearchWeb: webDef, types.ToolListPieces: fakeListPiecesTool().def}
	args, _ := json.Marshal(withNote("Recherche publique.", map[string]any{"query": "préavis de licenciement"}))
	rec := agentDecisionRecord{RawCalls: []u.ToolCallWire{
		{ID: "a", Name: types.ToolSearchWeb, Arguments: string(args)},
		{ID: "b", Name: types.ToolSearchWeb, Arguments: string(args)},
		{ID: "c", Name: types.ToolListPieces, Arguments: `{"progress_note":"Inventaire."}`},
	}}
	rec.Dispatch = 5
	agentValidateCalls(&rec, byName)
	agentRefuseOverBudget(&rec, &types.AgentTurn{WebSearches: types.MaxAgentWebSearches - 1})
	if _, refused := rec.Prefailed[0]; refused {
		t.Fatal("the last web search within budget was refused")
	}
	if rec.Prefailed[1].Code != types.ToolCodeBudgetExhausted {
		t.Fatalf("the over-budget search = %+v, want budget_exhausted", rec.Prefailed[1])
	}
	if _, refused := rec.Prefailed[2]; refused || rec.Rejection != "" {
		t.Fatal("an unmetered call and the decision itself must still run")
	}
}

func TestAgentContextEvictsWholeObservationsWithStubs(t *testing.T) {
	big := strings.Repeat("texte de la pièce ", 300)
	call := types.AgentToolCall{ID: "d1.c0", Name: types.ToolReadPiece, Arguments: json.RawMessage(`{}`)}
	data, _ := json.Marshal(map[string]string{"text": big})
	e := newObsEntry(call, types.ToolObservation{
		Status: types.ObservationOK, Data: data, EvidenceIDs: []string{"ev-1"}, ReadReceiptIDs: []string{"rr-1"},
	})
	if e.evicted || !strings.Contains(e.current(), "texte de la pièce") || len(e.receipts) != 1 {
		t.Fatal("an observation within bounds must be sent whole with its receipts")
	}
	if !strings.Contains(e.stub, `"evicted":true`) || !strings.Contains(e.stub, "ev-1") ||
		strings.Contains(e.stub, "texte de la pièce") || !strings.Contains(e.stub, "read_evidence") {
		t.Fatalf("the eviction stub must keep the evidence ids, drop the text and point at read_evidence: %s", e.stub)
	}

	prev := agentObservationChars
	agentObservationChars = 100
	defer func() { agentObservationChars = prev }()
	tooBig := newObsEntry(call, types.ToolObservation{Status: types.ObservationOK, Data: data, ReadReceiptIDs: []string{"rr-1"}})
	if !tooBig.evicted || len(tooBig.receipts) != 0 || strings.Contains(tooBig.current(), "texte de la pièce") {
		t.Fatal("an oversized observation is replaced whole, and nothing in it counts as read")
	}
}

func TestAgentAnswerRenderingKeepsPieceWarningsBesideTheirBlock(t *testing.T) {
	body := types.DocumentRevisionBody{Blocks: []types.DocumentBlock{
		{ID: "h", Role: types.BlockHeading, Level: 1, Text: "Chronologie"},
		{ID: "r1", Role: types.BlockChronologyRow, Text: "Embauche", Chronology: &types.ChronologyRow{
			Date: types.ClaimDate{PartialDate: types.PartialDate{Precision: types.DatePrecisionMonth, Year: 2023, Month: 3}}}},
		{ID: "r2", Role: types.BlockChronologyRow, Text: "Licenciement", Chronology: &types.ChronologyRow{
			Date: types.ClaimDate{PartialDate: types.PartialDate{Precision: types.DatePrecisionDay, Year: 2025, Month: 4, Day: 2}}}},
	}}
	const warning = "À vérifier — la date attribuée à la pièce n° 4 n'a pas été retrouvée dans le passage cité."
	report := types.VerificationReport{Issues: []types.VerificationIssue{
		{Code: types.IssuePieceDateNotFound, Severity: types.SeverityWarning, BlockID: "r2", Correction: warning},
		{Code: types.IssueLegalNotRetrieved, Severity: types.SeverityBlocking, BlockID: "r1", Correction: "jamais montré"},
	}}
	got := composeAnswerText(body, report, nil)
	lines := strings.Split(got, "\n\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "## Chronologie") || !strings.Contains(lines[2], "Licenciement") ||
		lines[3] != "> "+warning || strings.Contains(got, "jamais montré") {
		t.Fatalf("the warning must sit under its block and nothing blocking may show:\n%s", got)
	}
	chunks := agentChunks(strings.Repeat("mot ", 300))
	var joined strings.Builder
	for i, c := range chunks {
		if i > 0 && c.Start != chunks[i-1].End {
			t.Fatal("delivery chunks must be contiguous")
		}
		joined.WriteString(c.Text)
	}
	if joined.String() != strings.Repeat("mot ", 300) {
		t.Fatal("delivery chunks must reassemble into the text")
	}
}

func TestAgentExplanationsNeverInviteAFutileResend(t *testing.T) {
	turn := &types.AgentTurn{RunID: "abcdef0123456789", MaxDispatches: 40}
	for _, outcome := range []string{
		types.AgentOutcomeTurnLimit, types.AgentOutcomeDeadline, types.AgentOutcomeNoProgress,
		types.AgentOutcomeRefused, types.AgentOutcomeCancelled, types.AgentOutcomeFailed,
	} {
		text := agentExplanation(outcome, "", turn)
		lower := strings.ToLower(text)
		if text == "" || strings.Contains(lower, "renvoyez") || strings.Contains(lower, "réessayez") {
			t.Fatalf("%s: %q invites a resend", outcome, text)
		}
	}
}

// ---------------------------------------------------------------------------
// Review gaps
// ---------------------------------------------------------------------------

// A partial result opens with « its limits are stated below »: the gate
// refuses, back to the agent, a partial answer that states none, and only an
// answer carrying its limitation block is published.
func TestAgentPartialAnswerMustStateItsLimits(t *testing.T) {
	const limit = "Les pièces 4 à 18 n'ont pas été lues."
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		args := map[string]any{"answer": "Chronologie établie sur les pièces 1 à 3.", "outcome": types.AgentOutcomePartial}
		if n > 1 {
			args["limitation"] = limit
		}
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la chronologie partielle.", args)})
	}, submitTool())

	_, res := h.post(t, "Établis la chronologie des pièces 1 à 3.")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != 2 {
		t.Fatalf("run = %s/%s after %d dispatches, want succeeded after 2", run.State, run.FailureCode, h.gw.hits())
	}
	if !strings.Contains(h.gw.requestText(2), "indiquer ses limites") {
		t.Fatalf("dispatch 2 does not carry the refusal of a partial answer without limits: %s", h.gw.requestText(2))
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want only for the answer stating its limits", h.publisher.calls.Load())
	}
	msg := h.message(t, res.AssistantMessage.ID)
	if !strings.HasPrefix(msg.Content, agentPartialNotice) || !strings.Contains(msg.Content, "> Limite : "+limit) {
		t.Fatalf("delivered %q, want the partial notice followed by the stated limit", msg.Content)
	}
}

// scopeRefusingPublisher refuses as the real publisher does when the dossier
// moved after the gate: every publication of a refused answer revision. The
// first revision it sees is refused; with always set, every one is.
type scopeRefusingPublisher struct {
	fakePublisher
	always   bool
	mu       sync.Mutex
	refused  map[string]bool
	refusals atomic.Int32
}

func (p *scopeRefusingPublisher) PublishResult(ctx context.Context, in PublicationInput) (PublicationResult, error) {
	p.mu.Lock()
	if p.refused == nil {
		p.refused = map[string]bool{in.Answer.Revision.ID: true}
	}
	refuse := p.always || p.refused[in.Answer.Revision.ID]
	p.mu.Unlock()
	if refuse {
		p.refusals.Add(1)
		return PublicationResult{}, fmt.Errorf("%w (answer)", errPublicationScopeChanged)
	}
	return p.fakePublisher.PublishResult(ctx, in)
}

// A deterministic publication refusal is not a transient failure: it goes
// back to the agent as submit_result's observation, once, and the agent's
// resubmission is published.
func TestAgentPublicationRefusalGoesBackToTheAgent(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		answer := "Réponse vérifiée sur l'état précédent du dossier."
		if n > 1 {
			answer = "Réponse reprise sur le dossier à jour."
		}
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": answer})})
	}, submitTool())
	publisher := &scopeRefusingPublisher{}
	AgentPublisher = publisher

	_, res := h.post(t, "Combien de pièces compte le dossier ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != 2 {
		t.Fatalf("run = %s/%s after %d dispatches, want succeeded after 2", run.State, run.FailureCode, h.gw.hits())
	}
	if publisher.refusals.Load() != 1 {
		t.Fatalf("the refused publication was attempted %d times, want once (no transient retry)", publisher.refusals.Load())
	}
	if !strings.Contains(h.gw.requestText(2), types.ToolCodeScopeChanged) {
		t.Fatalf("dispatch 2 does not carry the publication refusal: %s", h.gw.requestText(2))
	}
	if msg := h.message(t, res.AssistantMessage.ID); msg.Content != "Réponse reprise sur le dossier à jour." {
		t.Fatalf("delivered %q, want the resubmitted answer", msg.Content)
	}
}

// A dossier that keeps moving ends the turn with its own explanation, which
// never says a resend is futile: once the pièces settle, it is not.
func TestAgentScopeChangeNeverCallsAResendFutile(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Le dossier compte trois pièces."})})
	}, submitTool())
	AgentPublisher = &scopeRefusingPublisher{always: true}

	_, res := h.post(t, "Combien de pièces compte le dossier ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateFailed || run.FailureCode != agentCodeScopeChanged {
		t.Fatalf("run = %s/%s, want failed/%s", run.State, run.FailureCode, agentCodeScopeChanged)
	}
	msg := h.message(t, res.AssistantMessage.ID)
	lower := strings.ToLower(msg.Content)
	for _, futile := range []string{"ne corrigera", "à l'identique", "ne changera rien", "ne permettrait", "même blocage"} {
		if strings.Contains(lower, futile) {
			t.Fatalf("the scope-change explanation calls a resend futile (%q): %q", futile, msg.Content)
		}
	}
	if !strings.Contains(lower, "dossier a été modifié") {
		t.Fatalf("the explanation does not say the dossier moved: %q", msg.Content)
	}
}

// A turn whose attachments were still being read at intake, and are read by
// the time bootstrap runs, starts streaming: the message leaves `reading` and
// attachments.read is emitted.
func TestAgentBootstrapStartsAMessageCreatedReading(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() { cleanupAgentRows(f.firm) })
	reg := &fakeRegistry{}
	if err := reg.Register(submitTool()); err != nil {
		t.Fatal(err)
	}
	prevTools := AgentTools
	AgentTools = reg
	t.Cleanup(func() { AgentTools = prevTools })

	// The rows intake writes for a turn whose attachment was still being read
	// — with no claimable run step, so no worker races the step called here.
	ctx := context.Background()
	repo := u.ForTenantID(ctx, f.firm)
	var piece types.Piece
	if err := repo.Pieces().Where("dossier_id = ?", f.dossier).Order("ordinal").Take(&piece).Error; err != nil {
		t.Fatalf("piece: %v", err)
	}
	conv := mustCreate(t, f, types.ConversationCreateRequest{DossierID: &f.dossier})
	runID := uuid.NewString()
	userMsg := types.Message{ID: uuid.NewString(), ConversationID: conv.ID, Role: types.MessageRoleUser,
		Content: "Que dit la pièce jointe ?", State: types.MessageStateComplete, Attachments: jsonbOfAny([]string{piece.ID})}
	assistant := types.Message{ID: uuid.NewString(), ConversationID: conv.ID, Role: types.MessageRoleAssistant,
		RunID: &runID, State: types.MessageStateReading}
	contract, _ := json.Marshal(types.AgentTaskContract{Version: types.AgentTaskContractVersion, Instruction: userMsg.Content,
		Scope: types.AgentSourceScope{Mode: types.ScopeAllPieces, DossierID: f.dossier}})
	turn := types.AgentTurn{ID: uuid.NewString(), RunID: runID, ActorID: f.user, RequestID: uuid.NewString(),
		RequestSHA256: "sha", ConversationID: &conv.ID, UserMessageID: &userMsg.ID, AssistantMessageID: &assistant.ID,
		DossierID: &f.dossier, MaxDispatches: 4, DeadlineAt: time.Now().Unix() + 60, ControllerModel: "m",
		PromptVersion: "p", ToolsetVersion: "t", TaskContract: types.JSONB(contract)}
	for _, row := range []any{&userMsg, &assistant, &turn} {
		if err := repo.Create(row); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	out, err := agentBootstrapStep(ctx, &runner.StepCtx{Run: types.Run{ID: runID}, Step: types.RunStep{StepKey: agentBootstrapKey}, TenantID: f.firm})
	exp, ok := out.(runner.Expansion)
	if err != nil || !ok || len(exp.Steps) != 1 || exp.Steps[0].Key != agentDecisionKey(1) {
		t.Fatalf("bootstrap = %T %+v, %v; want decision 1", out, out, err)
	}
	if err := exp.Commit(u.DB); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if msg := h0message(t, repo, assistant.ID); msg.State != types.MessageStateStreaming {
		t.Fatalf("assistant message = %s, want streaming", msg.State)
	}
	events, _ := readThreadEvents(repo, conv.ID, 0, 100)
	read := 0
	for _, ev := range events {
		if ev.Type == EventAttachmentsRead && payloadOf(ev)["pieces"] == float64(1) {
			read++
		}
	}
	if read != 1 {
		t.Fatalf("attachments.read events = %d, want 1 naming the attachment", read)
	}
}

func h0message(t *testing.T, repo *u.TenantRepo, id string) types.Message {
	t.Helper()
	var msg types.Message
	if err := repo.Messages().Where("id = ?", id).Take(&msg).Error; err != nil {
		t.Fatalf("message: %v", err)
	}
	return msg
}

// latestActivity is the last agent.activity row of each call of a thread.
func latestActivity(t *testing.T, h *agentHarness, convID string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, ev := range h.events(t, convID) {
		if ev.Type == types.EventAgentActivity {
			p := payloadOf(ev)
			out[fmt.Sprint(p["call_id"])] = p
		}
	}
	return out
}

// A run that ends while a call is in flight, or while ask_lawyer waits on the
// lawyer, closes that call's `running` row: same call id, status error, code
// interrupted.
func TestAgentRunEndClosesRunningActivity(t *testing.T) {
	t.Run("tool in flight", func(t *testing.T) {
		release := make(chan struct{})
		h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
			return 200, toolCallsResponse("", fakeCall{ID: "call_r", Name: types.ToolReadPiece, Args: note("Je lis la pièce 1.")})
		}, blockingReadTool(release), submitTool())
		conv, res := h.post(t, "Lis la pièce 1.")
		h.waitRun(t, res.Agent.RunID, func(types.Run) bool {
			var s types.RunStep
			return h.repo().RunSteps().Where("run_id = ? AND step_key = ?", res.Agent.RunID, agentToolKey(1, 0)).
				Take(&s).Error == nil && s.State == types.StepStateRunning
		}, "the tool to start")
		if status, out := CancelRun(ctxFor(h.f.firm, h.f.user), res.Agent.RunID); status != http.StatusOK {
			t.Fatalf("CancelRun: %d %v", status, out)
		}
		h.waitTerminal(t, res.Agent.RunID)
		close(release)
		p := latestActivity(t, h, conv.ID)["d1.c0"]
		if p["status"] != types.ActivityError || p["code"] != types.ActivityCodeInterrupted || p["tool"] != types.ToolReadPiece {
			t.Fatalf("the in-flight call's last row = %v, want error/interrupted", p)
		}
	})
	t.Run("question parked", func(t *testing.T) {
		h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
			return 200, toolCallsResponse("", fakeCall{ID: "call_q", Name: types.ToolAskLawyer,
				Args: withNote("Je vous pose une question.", map[string]any{"question": "La salariée était-elle cadre ?"})})
		}, askTool(), submitTool())
		conv, res := h.post(t, "Quel préavis ?")
		h.waitRun(t, res.Agent.RunID, func(r types.Run) bool { return r.State == types.RunStateWaitingInput }, "the question to park")
		if status, out := CancelRun(ctxFor(h.f.firm, h.f.user), res.Agent.RunID); status != http.StatusOK {
			t.Fatalf("CancelRun: %d %v", status, out)
		}
		h.waitTerminal(t, res.Agent.RunID)
		p := latestActivity(t, h, conv.ID)["d1.c0"]
		obj, _ := p["object"].(map[string]any)
		if p["status"] != types.ActivityError || p["code"] != types.ActivityCodeInterrupted || obj["kind"] != types.ActivityObjectQuestion {
			t.Fatalf("the parked question's last row = %v, want error/interrupted", p)
		}
	})
}

// Two answers that both passed validation while the question was pending
// serialise on the run lock: the second cannot overwrite the first. A
// different value is 409; the same value replayed is a no-op.
func TestAgentConcurrentAnswersCannotOverwriteEachOther(t *testing.T) {
	race := func(t *testing.T, first, second string) (*agentHarness, string, []int) {
		h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
			if n == 1 {
				return 200, toolCallsResponse("", fakeCall{ID: "call_q", Name: types.ToolAskLawyer,
					Args: withNote("Question.", map[string]any{"question": "Le salarié avait-il deux ans d'ancienneté ?"})})
			}
			return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
				Args: withNote("Soumission.", map[string]any{"answer": "Indemnité calculée."})})
		}, askTool(), submitTool())
		_, res := h.post(t, "Calcule l'indemnité.")
		runID := res.Agent.RunID
		h.waitRun(t, runID, func(r types.Run) bool { return r.State == types.RunStateWaitingInput }, "the question to park")

		// Hold the run lock so both requests validate against the pending
		// question, then queue on RecordAnswers' lock.
		hold := u.DB.Begin()
		if err := hold.Exec(`SELECT id FROM runs WHERE id = ? FOR UPDATE`, runID).Error; err != nil {
			t.Fatalf("lock: %v", err)
		}
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i, v := range []string{first, second} {
			wg.Add(1)
			go func(i int, v string) {
				defer wg.Done()
				statuses[i], _ = AnswerRun(ctxFor(h.f.firm, h.f.user), runID, types.AnswersRequest{Answers: []types.QuestionAnswer{
					{QuestionKey: agentQuestionKey(1, 1), Values: []string{v}},
				}})
			}(i, v)
			time.Sleep(150 * time.Millisecond) // the first queues on the lock before the second
		}
		time.Sleep(150 * time.Millisecond)
		hold.Rollback()
		wg.Wait()
		h.waitTerminal(t, runID)
		return h, runID, statuses
	}
	stored := func(t *testing.T, h *agentHarness, runID string) []string {
		var q types.RunQuestion
		h.repo().RunQuestions().Where("run_id = ? AND question_key = ?", runID, agentQuestionKey(1, 1)).Take(&q)
		var a struct {
			Values []string `json:"values"`
		}
		_ = json.Unmarshal(q.Answer, &a)
		return a.Values
	}

	t.Run("different value", func(t *testing.T) {
		h, runID, statuses := race(t, "oui", "non")
		if statuses[0] != http.StatusOK || statuses[1] != http.StatusConflict {
			t.Fatalf("statuses = %v, want the first recorded and the second 409", statuses)
		}
		if got := stored(t, h, runID); len(got) != 1 || got[0] != "oui" {
			t.Fatalf("stored answer = %v, want the first one", got)
		}
	})
	t.Run("same value replayed", func(t *testing.T) {
		h, runID, statuses := race(t, "oui", "oui")
		if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
			t.Fatalf("statuses = %v, want both 200", statuses)
		}
		var answers int64
		h.repo().RunEvents().Where("run_id = ? AND type = ?", runID, types.EventQuestionAnswer).Count(&answers)
		if answers != 1 {
			t.Fatalf("question.answer events = %d, want 1: the replay must be a no-op", answers)
		}
	})
}

// flakyTool fails its first `fails` executions with err, then succeeds.
func flakyTool(fails int, err error) (fakeTool, *atomic.Int32) {
	var n atomic.Int32
	tool := fakeListPiecesTool()
	inner := tool.exec
	tool.exec = func(ctx context.Context, x ToolExecution, args json.RawMessage) (ToolOutcome, error) {
		if int(n.Add(1)) <= fails {
			return ToolOutcome{}, err
		}
		return inner(ctx, x, args)
	}
	return tool, &n
}

// An error from a tool is retried while the step has attempts left; the turn
// ends on the last attempt, or at once for an error classified permanent.
func TestAgentToolErrorsAreRetriedUnlessPermanent(t *testing.T) {
	script := func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_l", Name: types.ToolListPieces, Args: note("Inventaire.")})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Trois pièces."})})
	}
	toolStep := func(t *testing.T, h *agentHarness, runID string) types.RunStep {
		var s types.RunStep
		if err := h.repo().RunSteps().Where("run_id = ? AND step_key = ?", runID, agentToolKey(1, 0)).Take(&s).Error; err != nil {
			t.Fatalf("tool step: %v", err)
		}
		return s
	}
	for _, tc := range []struct {
		name     string
		fails    int
		err      error
		state    string
		attempts int
	}{
		{"transient then ok", 1, errors.New("driver: bad connection"), types.RunStateSucceeded, 2},
		{"transient until the last attempt", 99, errors.New("driver: bad connection"), types.RunStateFailed, 3},
		{"permanent", 99, runner.Permanent("tool_defect", "the tool cannot run this call"), types.RunStateFailed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, execs := flakyTool(tc.fails, tc.err)
			h := newAgentHarness(t, script, tool, submitTool())
			_, res := h.post(t, "Inventaire du dossier ?")
			run := h.waitTerminal(t, res.Agent.RunID)
			if run.State != tc.state {
				t.Fatalf("run = %s/%s, want %s", run.State, run.FailureCode, tc.state)
			}
			if tc.state == types.RunStateFailed && run.FailureCode != agentCodeToolFailed {
				t.Fatalf("failure code = %s, want %s", run.FailureCode, agentCodeToolFailed)
			}
			if s := toolStep(t, h, run.ID); s.AttemptCount != tc.attempts || int(execs.Load()) != tc.attempts {
				t.Fatalf("tool ran %d times over %d attempts, want %d", execs.Load(), s.AttemptCount, tc.attempts)
			}
		})
	}
}

// A `running` row names its concrete object from the validated arguments —
// the query searched, the pièce read and from where, the document worked on,
// the questions asked — with the same public fields the completed row shows.
func TestAgentRunningActivityNamesItsObject(t *testing.T) {
	search := fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolSearchPieces, Description: "Recherche dans les pièces.",
			Parameters: fakeSchema(map[string]any{"query": map[string]any{"type": "string"}}, "query"),
			Class:      types.ToolClassExternal, ReadOnly: true,
		},
		exec: func(context.Context, ToolExecution, json.RawMessage) (ToolOutcome, error) {
			return ToolOutcome{Observation: types.ToolObservation{Status: types.ObservationEmpty}}, nil
		},
	}
	read := fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolReadPiece, Description: "Lecture d'une pièce.",
			Parameters: fakeSchema(map[string]any{"piece_id": map[string]any{"type": "string"}}, "piece_id"),
			Class:      types.ToolClassDB, ReadOnly: true,
		},
		exec: func(context.Context, ToolExecution, json.RawMessage) (ToolOutcome, error) {
			return ToolOutcome{Observation: types.ToolObservation{Status: types.ObservationOK}}, nil
		},
	}
	var piece types.Piece
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("",
				fakeCall{ID: "call_q", Name: types.ToolSearchPieces, Args: withNote("Je cherche le préavis.", map[string]any{"query": "durée du préavis"})},
				fakeCall{ID: "call_r", Name: types.ToolReadPiece, Args: withNote("Je lis le contrat.", map[string]any{"piece_id": piece.ID})})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Préavis d'un mois."})})
	}, search, read, submitTool())
	if err := h.repo().Pieces().Where("dossier_id = ?", h.f.dossier).Order("ordinal").Take(&piece).Error; err != nil {
		t.Fatalf("piece: %v", err)
	}

	conv, res := h.post(t, "Quel est le préavis ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	running := map[string]map[string]any{}
	for _, ev := range h.events(t, conv.ID) {
		if p := payloadOf(ev); ev.Type == types.EventAgentActivity && p["status"] == types.ActivityRunning {
			running[fmt.Sprint(p["call_id"])], _ = p["object"].(map[string]any)
		}
	}
	if obj := running["d1.c0"]; obj["kind"] != types.ActivityObjectQuery || obj["label"] != "durée du préavis" {
		t.Fatalf("running search row = %v, want the query", obj)
	}
	if obj := running["d1.c1"]; obj["kind"] != types.ActivityObjectPiece || obj["label"] != pieceLabel(piece) || obj["detail"] == "" {
		t.Fatalf("running read row = %v, want %q and its range", obj, pieceLabel(piece))
	}

	// The document and question tools, on the same turn.
	turn := h.turn(t, run.ID)
	rev := types.AgentDocumentRevision{ID: uuid.NewString(), RunID: run.ID, DocumentKey: "note", Revision: 2,
		Kind: types.AgentOutputLegalNote, Title: "Note sur le préavis", ContentSHA256: "sha", SchemaVersion: types.DocumentSchemaVersion,
		Body: types.JSONB(`{}`), CreatedByCallID: "d9.c0"}
	if err := h.repo().Create(&rev); err != nil {
		t.Fatalf("revision: %v", err)
	}
	object := func(tool string, args map[string]any) (types.AgentActivityObject, *types.AgentActivityCounts) {
		raw, _ := json.Marshal(withNote("Note.", args))
		return agentRunningObject(context.Background(), h.repo(), &turn, types.AgentToolCall{ID: "d9.c0", Name: tool, Arguments: raw},
			func() (PieceScopeSnapshot, bool) { return PieceScopeSnapshot{}, false })
	}
	for _, tc := range []struct {
		tool, label, detail string
		args                map[string]any
	}{
		{types.ToolWriteDocument, "Synthèse des griefs", "révision 1", map[string]any{"title": "Synthèse des griefs"}},
		{types.ToolEditDocument, rev.Title, "révision 2", map[string]any{"revision_id": rev.ID, "edits": []map[string]any{{"op": "delete", "block_id": "b1"}}}},
		{types.ToolReadDocument, rev.Title, "révision 2", map[string]any{"document_key": "note"}},
		{types.ToolVerifyOutput, rev.Title, "", map[string]any{"revision_id": rev.ID}},
		{types.ToolReviewOutput, rev.Title, "", map[string]any{"revision_id": rev.ID}},
		{types.ToolSearchLaw, "préavis de licenciement", "Légifrance", map[string]any{"query": "préavis de licenciement"}},
		{types.ToolEditDocument, "", "", map[string]any{"revision_id": "rev-of-another-run", "edits": []map[string]any{{"op": "delete", "block_id": "b1"}}}},
		{types.ToolEditDocument, "", "", map[string]any{"revision_id": rev.ID, "edits": []map[string]any{}}},
	} {
		obj, _ := object(tc.tool, tc.args)
		if obj.Kind != activityKindFor(tc.tool) || obj.Label != tc.label || obj.Detail != tc.detail {
			t.Fatalf("%s running object = %+v, want label %q detail %q", tc.tool, obj, tc.label, tc.detail)
		}
	}
	question := func(key, prompt string) map[string]any {
		return map[string]any{"key": key, "prompt": prompt, "kind": types.QuestionKindText, "options": nil, "required": true, "decision_key": nil}
	}
	obj, counts := object(types.ToolAskLawyer, map[string]any{"questions": []map[string]any{
		question("cadre", "La salariée était-elle cadre ?"), question("ccn", "Quelle convention collective ?"),
	}})
	if obj.Label != "La salariée était-elle cadre ?" || counts == nil || counts.Questions == nil || *counts.Questions != 2 {
		t.Fatalf("ask_lawyer running row = %+v %+v, want the first question and a count of 2", obj, counts)
	}
}

// A failed call's final activity row carries its observation code — and
// only the code: a public query the confidentiality gate kept inside the
// cabinet is told apart from one that left and failed.
func TestAgentFailedActivityCarriesItsCode(t *testing.T) {
	web := fakeTool{
		def: types.AgentToolDefinition{
			Name: types.ToolSearchWeb, Description: "Recherche en sources ouvertes.",
			Parameters: fakeSchema(map[string]any{"query": map[string]any{"type": "string"}}, "query"),
			Class:      types.ToolClassExternal, ReadOnly: true,
		},
		exec: func(_ context.Context, x ToolExecution, _ json.RawMessage) (ToolOutcome, error) {
			act := types.AgentActivityObject{Kind: types.ActivityObjectQuery, Label: "Société Exemple licenciement"}
			return failure(x, types.ToolSearchWeb, types.ToolCodeUnsafePublicQuery, false, "requête refusée",
				map[string]any{"category": "party"}, act), nil
		},
	}
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		if n == 1 {
			return 200, toolCallsResponse("", fakeCall{ID: "call_w", Name: types.ToolSearchWeb,
				Args: withNote("Je cherche en sources ouvertes.", map[string]any{"query": "Société Exemple licenciement"})})
		}
		return 200, toolCallsResponse("", fakeCall{ID: "call_s", Name: types.ToolSubmitResult,
			Args: withNote("Soumission.", map[string]any{"answer": "Aucune recherche nominative n'a été faite."})})
	}, web, submitTool())

	conv, res := h.post(t, "Cherche la Société Exemple sur le web.")
	if run := h.waitTerminal(t, res.Agent.RunID); run.State != types.RunStateSucceeded {
		t.Fatalf("run = %s/%s", run.State, run.FailureCode)
	}
	p := latestActivity(t, h, conv.ID)["d1.c0"]
	if p["status"] != types.ActivityError || p["code"] != types.ToolCodeUnsafePublicQuery {
		t.Fatalf("the refused query's last row = %v, want error/%s", p, types.ToolCodeUnsafePublicQuery)
	}
}
