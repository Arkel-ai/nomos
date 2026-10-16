package services

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Request idempotency, on every entry point of
// the agent intake: the same request id with the same canonical request
// returns the committed result — same thread, messages and run —, another
// request under that id is 409 request_conflict, two identical requests
// racing each other leave one row set (the unique index decides, not a
// lookup), and a request without an id gets one minted and returned. No
// runner is started: what is under test is what the intake commits.

// newIntakeFixture enables the agent intake without running it.
func newIntakeFixture(t *testing.T) *convFixture {
	t.Helper()
	f := newFixture(t)
	t.Cleanup(func() { cleanupAgentRows(f.firm) })
	newScriptedGateway(t, func(int, map[string]any) (int, string) { return http.StatusInternalServerError, `{}` })
	registry := &fakeRegistry{}
	if err := registry.Register(submitTool()); err != nil {
		t.Fatal(err)
	}
	prevTools, prevVerify, prevPublisher := AgentTools, VerifyOutput, AgentPublisher
	AgentTools, VerifyOutput, AgentPublisher = registry, passingVerifier(0), &fakePublisher{}
	t.Cleanup(func() {
		AgentTools, VerifyOutput, AgentPublisher = prevTools, prevVerify, prevPublisher
	})
	return f
}

func (f *convFixture) count(t *testing.T, table string) int64 {
	t.Helper()
	return testCount(t, table, "tenant_id = ?", f.firm)
}

func (f *convFixture) wantRows(t *testing.T, conversations, turns, runs, messages int64) {
	t.Helper()
	got := [4]int64{f.count(t, "conversations"), f.count(t, "agent_turns"), f.count(t, "runs"), f.count(t, "messages")}
	if got != [4]int64{conversations, turns, runs, messages} {
		t.Fatalf("rows (conversations, turns, runs, messages) = %v, want %v", got, [4]int64{conversations, turns, runs, messages})
	}
}

func wantConflict(t *testing.T, status int, out any) {
	t.Helper()
	body, _ := out.(map[string]any)
	if status != http.StatusConflict || body["code"] != IntakeRequestConflict || body["detail"] == "" {
		t.Fatalf("got %d %v, want 409 request_conflict", status, out)
	}
}

func createWithTurn(t *testing.T, f *convFixture, user string, body types.ConversationCreateRequest) (int, types.ConversationDetail, any) {
	t.Helper()
	status, out := CreateConversation(ctxFor(f.firm, user), body)
	detail, _ := out.(types.ConversationDetail)
	return status, detail, out
}

func TestAgentIntakeConversationCreationIsIdempotent(t *testing.T) {
	f := newIntakeFixture(t)
	rid := uuid.NewString()
	body := types.ConversationCreateRequest{DossierID: &f.dossier, RequestID: rid,
		Message: &types.MessageCreateRequest{Content: "Résume le dossier."}}

	status, first, out := createWithTurn(t, f, f.user, body)
	if status != http.StatusCreated || first.RequestID != rid || len(first.Messages) != 2 || len(first.Runs) != 1 {
		t.Fatalf("creation: %d %+v", status, out)
	}
	f.wantRows(t, 1, 1, 1, 2)

	// A network retry: the thread already created, never a second one.
	status, again, out := createWithTurn(t, f, f.user, body)
	if status != http.StatusCreated || again.ID != first.ID || again.RequestID != rid ||
		again.Messages[0].ID != first.Messages[0].ID || again.Messages[1].ID != first.Messages[1].ID ||
		again.Runs[0].ID != first.Runs[0].ID {
		t.Fatalf("replay: %d %+v", status, out)
	}
	f.wantRows(t, 1, 1, 1, 2)

	// The same id with another request is a conflict, and writes nothing.
	other := body
	other.Message = &types.MessageCreateRequest{Content: "Établis la chronologie."}
	status, _, out = createWithTurn(t, f, f.user, other)
	wantConflict(t, status, out)
	f.wantRows(t, 1, 1, 1, 2)

	// Another member of the firm reusing the id never receives this thread.
	colleague := "test_user_conv_" + uuid.NewString()
	if err := u.AddRoleForUserInDomain(colleague, types.RoleUser, f.firm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.DB.Exec("DELETE FROM casbin_rule WHERE v0 = ?", colleague) })
	status, _, out = createWithTurn(t, f, colleague, body)
	wantConflict(t, status, out)

	// No id: one is minted, returned and stored.
	status, minted, out := createWithTurn(t, f, f.user, types.ConversationCreateRequest{
		Message: &types.MessageCreateRequest{Content: "Bonjour."}})
	if status != http.StatusCreated || uuid.Validate(minted.RequestID) != nil {
		t.Fatalf("minted request id: %d %+v", status, out)
	}
	if n := testCount(t, "agent_turns", "tenant_id = ? AND request_id = ? AND conversation_id = ?",
		f.firm, minted.RequestID, minted.ID); n != 1 {
		t.Fatalf("the minted id is not the stored one (%d rows)", n)
	}
	f.wantRows(t, 2, 2, 2, 4)
}

func TestAgentIntakeConcurrentCreationsLeaveOneThread(t *testing.T) {
	f := newIntakeFixture(t)
	body := types.ConversationCreateRequest{DossierID: &f.dossier, RequestID: uuid.NewString(),
		Message: &types.MessageCreateRequest{Content: "Quelles pièces manquent au dossier ?"}}

	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, detail, out := createWithTurn(t, f, f.user, body)
			if status != http.StatusCreated {
				t.Errorf("concurrent creation %d: %d %v", i, status, out)
				return
			}
			ids[i] = detail.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent identical requests answered different threads: %v", ids)
		}
	}
	f.wantRows(t, 1, 1, 1, 2)
}

func TestAgentIntakeMessageIsIdempotent(t *testing.T) {
	f := newIntakeFixture(t)
	conv := mustCreate(t, f, types.ConversationCreateRequest{})
	other := types.Dossier{ID: uuid.NewString(), Title: "Autre dossier", State: types.DossierStateActive, CreatedBy: f.user}
	if err := u.ForTenantID(context.Background(), f.firm).Create(&other); err != nil {
		t.Fatal(err)
	}
	rid := uuid.NewString()
	body := types.MessageCreateRequest{Content: "Résume le contrat de travail.", DossierID: &f.dossier, RequestID: rid}
	post := func(b types.MessageCreateRequest) (int, types.MessageCreateResponse, any) {
		status, out := PostMessage(ctxFor(f.firm, f.user), conv.ID, b)
		res, _ := out.(types.MessageCreateResponse)
		return status, res, out
	}
	boundTo := func() string {
		var c types.Conversation
		if err := u.ForTenantID(context.Background(), f.firm).Conversations().Where("id = ?", conv.ID).Take(&c).Error; err != nil {
			t.Fatal(err)
		}
		if c.DossierID == nil {
			return ""
		}
		return *c.DossierID
	}

	status, first, out := post(body)
	if status != http.StatusCreated || first.RequestID != rid || first.Agent == nil || first.Cursor == 0 {
		t.Fatalf("first post: %d %+v", status, out)
	}
	if boundTo() != f.dossier {
		t.Fatal("the turn did not bind its collection")
	}

	// The retry finds the committed turn — not the 409 active_turn its own
	// still-running run would otherwise cause.
	status, again, out := post(body)
	if status != http.StatusCreated || again.UserMessage.ID != first.UserMessage.ID ||
		again.AssistantMessage.ID != first.AssistantMessage.ID || *again.RunID != *first.RunID ||
		again.Cursor != first.Cursor || again.RequestID != rid {
		t.Fatalf("replay: %d %+v, first %+v", status, out, first)
	}
	f.wantRows(t, 1, 1, 1, 2)

	// Another request under the same id: refused, and its binding is not applied.
	conflicting := body
	conflicting.DossierID = &other.ID
	status, _, out = post(conflicting)
	wantConflict(t, status, out)
	if boundTo() != f.dossier {
		t.Fatal("a refused request rebound the thread")
	}

	// A new send with a new id while the turn runs is still one active turn.
	status, _, out = post(types.MessageCreateRequest{Content: "Et la lettre de licenciement ?", RequestID: uuid.NewString()})
	if b, _ := out.(map[string]any); status != http.StatusConflict || b["code"] != IntakeActiveTurn {
		t.Fatalf("a second turn: %d %v, want 409 active_turn", status, out)
	}

	// Concurrent retries of the committed request all replay it.
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, res, out := post(body); status != http.StatusCreated || *res.RunID != *first.RunID {
				t.Errorf("concurrent replay: %d %v", status, out)
			}
		}()
	}
	wg.Wait()
	f.wantRows(t, 1, 1, 1, 2)
}

// Concurrent first sends of one message to one thread: the thread lock and
// the claim leave exactly one turn.
func TestAgentIntakeConcurrentMessagesLeaveOneTurn(t *testing.T) {
	f := newIntakeFixture(t)
	conv := mustCreate(t, f, types.ConversationCreateRequest{DossierID: &f.dossier})
	body := types.MessageCreateRequest{Content: "Liste les pièces.", RequestID: uuid.NewString()}
	runs := make([]string, 6)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, out := PostMessage(ctxFor(f.firm, f.user), conv.ID, body)
			res, ok := out.(types.MessageCreateResponse)
			if status != http.StatusCreated || !ok {
				t.Errorf("concurrent post %d: %d %v", i, status, out)
				return
			}
			runs[i] = *res.RunID
		}()
	}
	wg.Wait()
	for _, r := range runs {
		if r != runs[0] {
			t.Fatalf("concurrent identical posts answered different runs: %v", runs)
		}
	}
	f.wantRows(t, 1, 1, 1, 2)
}
