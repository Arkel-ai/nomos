package services

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The conversation layer, tested against a REAL Postgres. Everything worth
// asserting here is either SQL (cursor ordering under concurrent writers,
// tenant scoping) or transport (a stream that survives its viewer hanging up).
//
// The load-bearing assertion is TestConversationStreamSurvivesDisconnect: a
// dropped SSE connection is LOSS OF OBSERVATION, NEVER CANCELLATION. If that
// one ever goes red, the invariant in the package comment is broken and the
// product's whole durability story with it.

const testDSN = "postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"

var (
	convEnvOnce sync.Once
	dbReady     bool
)

// setupConversationSuite is what a TestMain would do, except that another test
// file in this package already owns TestMain — a package gets exactly one. It
// is idempotent and every test in this file calls it first.
func setupConversationSuite() {
	convEnvOnce.Do(func() {
		gin.SetMode(gin.TestMode)
		// The real key and the real DSN live in api/.env (gitignored). Load
		// does not overwrite anything already exported, so CI stays in
		// control; under NOMOS_TEST_NO_DOTENV=1 neither the file nor the
		// developer database is ever used.
		testmode.LoadDotenv("../.env")
		if dsn := testmode.DatabaseURL(testDSN); dsn != "" {
			os.Setenv("DATABASE_URL", dsn)
		}
		if u.DB == nil {
			u.InitClients(config.Load())
		}
		dbReady = u.DB != nil && u.Enforcer != nil

		// Compress the clock: these tests assert ordering and durability, not
		// latency.
		StreamPollInterval = 40 * time.Millisecond
		StreamHeartbeat = 2 * time.Second
	})
}

// The services suites honour the test modes: under NOMOS_TEST_NO_DOTENV=1
// neither api/.env nor the developer database is used, and under
// NOMOS_REQUIRE_DEPENDENCIES=1 a suite whose database is missing FAILS
// instead of skipping. The suite runs in a child process of this test
// binary, with no DATABASE_URL.
func TestServicesSuitesHonourTheTestModes(t *testing.T) {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DATABASE_URL=") && !strings.HasPrefix(kv, "NOMOS_") {
			env = append(env, kv)
		}
	}
	env = append(env, "NOMOS_TEST_NO_DOTENV=1", "NOMOS_REQUIRE_DEPENDENCIES=1", "NOMOS_LIVE=0")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOfficialRevalidationRefusesAChangedText$", "-test.count=1", "-test.v")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "required dependency missing") {
		t.Fatalf("the suite did not fail on its missing database (err=%v):\n%s", err, out)
	}
	if strings.Contains(string(out), "Postgres (GORM) connected") {
		t.Fatalf("the suite connected to a database it was not given:\n%s", out)
	}
}

// --- fixture ----------------------------------------------------------------

type convFixture struct {
	firm    string
	user    string
	dossier string
}

func newFixture(t *testing.T) *convFixture {
	t.Helper()
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres/Casbin not reachable — conversation suite skipped")
	}
	f := &convFixture{
		firm: "test_firm_conv_" + uuid.NewString(),
		user: "test_user_conv_" + uuid.NewString(),
	}
	ctx := context.Background()
	if err := u.EnsureFirm(ctx, f.firm, "Cabinet de test", f.user); err != nil {
		t.Fatalf("EnsureFirm: %v", err)
	}
	if err := u.AddRoleForUserInDomain(f.user, types.RoleUser, f.firm); err != nil {
		t.Fatalf("attach user: %v", err)
	}

	repo := u.ForTenantID(ctx, f.firm)
	d := types.Dossier{
		ID:        uuid.NewString(),
		Title:     "Prud'hommes — dossier de test",
		State:     types.DossierStateActive,
		CreatedBy: f.user,
	}
	if err := repo.Create(&d); err != nil {
		t.Fatalf("seed dossier: %v", err)
	}
	f.dossier = d.ID
	// A dossier with pieces: an empty collection is a legitimate reason for
	// the router to answer instead of planning, and that is not what the
	// mission tests are about.
	for i := 1; i <= 3; i++ {
		asset := types.Asset{
			ID:     uuid.NewString(),
			S3Key:  u.S3Prefix + f.firm + "/" + d.ID + "/" + uuid.NewString() + ".pdf",
			Kind:   types.AssetKindOriginal,
			Mime:   "application/pdf",
			SHA256: strings.Repeat("a", 64),
		}
		if err := repo.Create(&asset); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		if err := repo.Create(&types.Piece{
			ID:                  uuid.NewString(),
			DossierID:           d.ID,
			SourceAssetID:       asset.ID,
			OriginalName:        fmt.Sprintf("piece-%d.pdf", i),
			Ordinal:             i,
			PieceKind:           "contrat",
			ClassificationState: types.PieceStateProposed,
		}); err != nil {
			t.Fatalf("seed piece: %v", err)
		}
	}

	t.Cleanup(func() { f.cleanup(t) })
	return f
}

func (f *convFixture) cleanup(t *testing.T) {
	t.Helper()
	db := u.DB
	for _, table := range []string{
		"llm_calls", "run_events", "run_questions", "step_attempts",
		"run_steps", "runs", "messages", "conversations", "artifacts",
		"pieces", "assets", "audit_events", "dossiers",
	} {
		db.Exec(fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ?", table), f.firm)
	}
	db.Exec("DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ?", f.firm, f.firm)
	db.Exec("DELETE FROM firms WHERE id = ?", f.firm)
	db.Exec("DELETE FROM casbin_rule WHERE v0 = ?", f.user)
	if err := u.Enforcer.LoadPolicy(); err != nil {
		t.Logf("reloading casbin policy: %v", err)
	}
}

// ctxFor builds the gin context RequireAuth + RequireTenant would have built.
// What is under test is everything that happens AFTER authentication.
func ctxFor(firm, user string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/conversations", nil).
		WithContext(context.Background())
	c.Set(middlewares.AuthUserKey, &u.AuthUser{ID: user, Email: user + "@example.test"})
	c.Set(u.TenantContextKey, firm)
	return c
}

func mustCreate(t *testing.T, f *convFixture, body types.ConversationCreateRequest) types.Conversation {
	t.Helper()
	status, out := CreateConversation(ctxFor(f.firm, f.user), body)
	if status != http.StatusCreated {
		t.Fatalf("CreateConversation: status %d body %v", status, out)
	}
	conv, ok := out.(types.Conversation)
	if !ok {
		t.Fatalf("CreateConversation returned %T, want types.Conversation", out)
	}
	return conv
}

// --- CRUD -------------------------------------------------------------------

func TestConversationCRUD(t *testing.T) {
	f := newFixture(t)

	conv := mustCreate(t, f, types.ConversationCreateRequest{Title: "Dossier Martin"})
	if conv.TenantID != f.firm || conv.CreatedBy != f.user {
		t.Fatalf("conversation stamped wrong: tenant=%s created_by=%s", conv.TenantID, conv.CreatedBy)
	}

	// The history rail.
	status, out := ListConversations(ctxFor(f.firm, f.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("ListConversations: status %d body %v", status, out)
	}
	list := out.(types.ListResponse)
	if list.Total != 1 {
		t.Fatalf("history rail total = %d, want 1", list.Total)
	}

	// The thread.
	status, out = GetConversation(ctxFor(f.firm, f.user), conv.ID)
	if status != http.StatusOK {
		t.Fatalf("GetConversation: status %d body %v", status, out)
	}
	detail := out.(types.ConversationDetail)
	if len(detail.Messages) != 0 || len(detail.Runs) != 0 {
		t.Fatalf("fresh thread is not empty: %d messages, %d runs", len(detail.Messages), len(detail.Runs))
	}

	// Rename + bind a collection.
	title := "Contentieux Martin / Sté Dupont"
	status, out = UpdateConversation(ctxFor(f.firm, f.user), conv.ID, types.ConversationUpdateRequest{
		Title:     &title,
		DossierID: ref(&f.dossier),
	})
	if status != http.StatusOK {
		t.Fatalf("UpdateConversation: status %d body %v", status, out)
	}
	updated := out.(types.Conversation)
	if updated.Title != title || updated.DossierID == nil || *updated.DossierID != f.dossier {
		t.Fatalf("rename/bind did not stick: %+v", updated)
	}

	// Explicit null unbinds; an absent key would have left it alone.
	var none *string
	status, out = UpdateConversation(ctxFor(f.firm, f.user), conv.ID, types.ConversationUpdateRequest{
		DossierID: ref(none),
	})
	if status != http.StatusOK {
		t.Fatalf("unbind: status %d body %v", status, out)
	}
	if out.(types.Conversation).DossierID != nil {
		t.Fatalf("explicit null did not unbind the collection")
	}

	if status, out = DeleteConversation(ctxFor(f.firm, f.user), conv.ID); status != http.StatusOK {
		t.Fatalf("DeleteConversation: status %d body %v", status, out)
	}
	if status, _ = GetConversation(ctxFor(f.firm, f.user), conv.ID); status != http.StatusNotFound {
		t.Fatalf("deleted conversation still readable: status %d", status)
	}
}

// TestConversationCreateWithFirstTurn pins the shape the composer navigates
// on. Creating a thread WITH its first turn must answer with the THREAD — the
// conversation id included — not with the turn alone: a response carrying only
// the two messages sent the client to /chat/undefined and a 404.
func TestConversationCreateWithFirstTurn(t *testing.T) {
	f := newFixture(t)

	status, out := CreateConversation(ctxFor(f.firm, f.user), types.ConversationCreateRequest{
		DossierID: &f.dossier,
		Message:   &types.MessageCreateRequest{Content: "Peux-tu me résumer où en est ce dossier ?"},
	})
	if status != http.StatusCreated {
		t.Fatalf("CreateConversation with first turn: status %d body %v", status, out)
	}
	detail, ok := out.(types.ConversationDetail)
	if !ok {
		t.Fatalf("CreateConversation returned %T, want types.ConversationDetail", out)
	}
	if detail.ID == "" {
		t.Fatalf("response carries no conversation id: %+v", detail)
	}
	if len(detail.Messages) == 0 || detail.Messages[0].Role != types.MessageRoleUser {
		t.Fatalf("first turn missing from the thread: %+v", detail.Messages)
	}
	// The id the client would route to must be readable.
	if status, out = GetConversation(ctxFor(f.firm, f.user), detail.ID); status != http.StatusOK {
		t.Fatalf("thread not readable at the returned id: status %d body %v", status, out)
	}
}

// TestConversationCrossTenantIsolation plays the attacker: another firm, a
// valid uuid, every entry point. Nothing may leak, and nothing may answer 403
// either — a 403 confirms the uuid exists (§6.1).
func TestConversationCrossTenantIsolation(t *testing.T) {
	victim := newFixture(t)
	attacker := newFixture(t)

	conv := mustCreate(t, victim, types.ConversationCreateRequest{
		Title:     "SECRET — stratégie de firme A",
		DossierID: &victim.dossier,
	})

	c := ctxFor(attacker.firm, attacker.user)
	if status, _ := GetConversation(c, conv.ID); status != http.StatusNotFound {
		t.Errorf("GET thread across tenants: status %d, want 404", status)
	}
	if status, _ := UpdateConversation(c, conv.ID, types.ConversationUpdateRequest{}); status != http.StatusNotFound {
		t.Errorf("PATCH thread across tenants: status %d, want 404", status)
	}
	if status, _ := PostMessage(c, conv.ID, types.MessageCreateRequest{Content: "coucou"}); status != http.StatusNotFound {
		t.Errorf("POST message across tenants: status %d, want 404", status)
	}
	if status, _ := ConversationEvents(c, conv.ID, types.EventsQuery{}); status != http.StatusNotFound {
		t.Errorf("GET events across tenants: status %d, want 404", status)
	}
	if status, _ := DeleteConversation(c, conv.ID); status != http.StatusNotFound {
		t.Errorf("DELETE thread across tenants: status %d, want 404", status)
	}

	// And the victim's own list never mentions it.
	status, out := ListConversations(c, types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("attacker list: status %d", status)
	}
	if body, _ := json.Marshal(out); strings.Contains(string(body), conv.ID) {
		t.Errorf("attacker's history rail contains the victim's thread id")
	}
}

// TestConversationCursorOrdering hammers one thread from many goroutines while
// a reader walks the cursor. run_events.id is a bigserial: without the
// per-thread advisory lock two writers can take ids 10 and 11 and commit in
// the opposite order, and a reader that advanced past 11 never sees 10 again.
// That hole is exactly what a reconnecting client cannot survive.
func TestConversationCursorOrdering(t *testing.T) {
	f := newFixture(t)
	conv := mustCreate(t, f, types.ConversationCreateRequest{Title: "Cursor"})

	const writers, perWriter = 8, 25
	const want = writers * perWriter

	repo := u.ForTenantID(context.Background(), f.firm)
	seen := map[uint64]bool{}
	tags := map[string]bool{}
	done := make(chan struct{})

	// A reader that advances its cursor WHILE the writers are running — the
	// only way the race can be observed at all.
	go func() {
		defer close(done)
		var after uint64
		deadline := time.After(30 * time.Second)
		for len(seen) < want {
			select {
			case <-deadline:
				return
			default:
			}
			events, err := readThreadEvents(repo, conv.ID, after, 50)
			if err != nil {
				t.Errorf("cursor read: %v", err)
				return
			}
			for _, ev := range events {
				if ev.ID <= after {
					t.Errorf("cursor went backwards: %d after %d", ev.ID, after)
				}
				after = ev.ID
				seen[ev.ID] = true
				var payload struct {
					Tag string `json:"tag"`
				}
				_ = json.Unmarshal(ev.Payload, &payload)
				tags[payload.Tag] = true
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := emitThreadEvent(context.Background(), f.firm, conv.ID, nil,
					types.EventStepProgress, map[string]any{"tag": fmt.Sprintf("%d-%d", w, i)}); err != nil {
					t.Errorf("emit: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	<-done

	if len(seen) != want {
		t.Fatalf("cursor delivered %d events, want %d — the log has a hole", len(seen), want)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			if !tags[fmt.Sprintf("%d-%d", w, i)] {
				t.Fatalf("event %d-%d never reached the cursor", w, i)
			}
		}
	}
}

// --- routing ----------------------------------------------------------------

// A dossier the composer opened around dropped files carries a placeholder
// title; the turn that sent the files names it, from the instruction and the
// file names, and says so on the thread.
func TestComposerDossierIsNamedFromItsTurn(t *testing.T) {
	f := newFixture(t)
	if !u.LLMConfigured() {
		t.Skip("no gateway key — naming suite skipped")
	}
	ctx := context.Background()
	repo := u.ForTenantID(ctx, f.firm)
	d := types.Dossier{
		ID:           uuid.NewString(),
		Title:        "Nouveau dossier",
		State:        types.DossierStateActive,
		TitlePending: true,
		CreatedBy:    f.user,
	}
	if err := repo.Create(&d); err != nil {
		t.Fatalf("seed dossier: %v", err)
	}
	for i, name := range []string{
		"01-contrat-de-travail-cdi-2019.pdf",
		"05-lettre-de-licenciement-vasseur.pdf",
		"16-attestation-temoin-arbogast.docx",
	} {
		asset := types.Asset{
			ID:     uuid.NewString(),
			S3Key:  u.S3Prefix + f.firm + "/" + d.ID + "/" + uuid.NewString(),
			Kind:   types.AssetKindOriginal,
			Mime:   "application/pdf",
			SHA256: strings.Repeat("b", 64),
		}
		if err := repo.Create(&asset); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		if err := repo.Create(&types.Piece{
			ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: asset.ID,
			OriginalName: name, Ordinal: i + 1, ClassificationState: types.PieceStatePending,
		}); err != nil {
			t.Fatalf("seed piece: %v", err)
		}
	}
	conv := mustCreate(t, f, types.ConversationCreateRequest{DossierID: &d.ID})

	nameDossier(ctx, f.firm, conv.ID, d.ID,
		"Classe les pièces de ce dossier prud'homal et prépare le bordereau.")

	var named types.Dossier
	if err := repo.Dossiers().Where("id = ?", d.ID).Take(&named).Error; err != nil {
		t.Fatalf("dossier: %v", err)
	}
	if named.TitlePending || named.Title == "" || named.Title == d.Title {
		t.Fatalf("the dossier was not named: %+v", named)
	}
	var events int64
	repo.RunEvents().Where("conversation_id = ? AND type = ?", conv.ID, EventDossierTitled).Count(&events)
	if events != 1 {
		t.Fatalf("want one %s event on the thread, got %d", EventDossierTitled, events)
	}
	t.Logf("named %q", named.Title)
}

// A dossier the lawyer titled is never renamed, and asking costs nothing.
func TestTitledDossierIsNeverRenamed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	repo := u.ForTenantID(ctx, f.firm)
	conv := mustCreate(t, f, types.ConversationCreateRequest{DossierID: &f.dossier})

	nameDossier(ctx, f.firm, conv.ID, f.dossier, "Résume ce dossier.")

	var d types.Dossier
	if err := repo.Dossiers().Where("id = ?", f.dossier).Take(&d).Error; err != nil {
		t.Fatalf("dossier: %v", err)
	}
	if d.Title != "Prud'hommes — dossier de test" {
		t.Fatalf("a titled dossier was renamed to %q", d.Title)
	}
	var calls int64
	repo.LLMCalls().Count(&calls)
	if calls != 0 {
		t.Fatalf("naming a titled dossier made %d model calls", calls)
	}
}

// TestConversationStreamSurvivesDisconnect: a writer appends a reply's deltas
// to the durable log the way the agent's delivery step does, while a first
// connection reads one delta and hangs up. A second connection resumes from
// Last-Event-ID and must replay the rest exactly once, through
// message.complete — the writer never knew anyone had left.
func TestConversationStreamSurvivesDisconnect(t *testing.T) {
	f := newFixture(t)

	srv := httptest.NewServer(streamRouter(f))
	defer srv.Close()

	conv := mustCreate(t, f, types.ConversationCreateRequest{DossierID: &f.dossier})
	cursor := conversationCursor(u.ForTenantID(context.Background(), f.firm), conv.ID)
	messageID := uuid.NewString()
	parts := []string{"Premier point. ", "Deuxième point. ", "Troisième point. ", "Dernier point."}

	var writeErr error
	written := make(chan struct{})
	go func() {
		defer close(written)
		ctx := context.Background()
		for i, delta := range parts {
			time.Sleep(50 * time.Millisecond)
			if _, writeErr = emitThreadEvent(ctx, f.firm, conv.ID, nil, types.EventMessageDelta,
				map[string]any{"message_id": messageID, "seq": i + 1, "delta": delta}); writeErr != nil {
				return
			}
		}
		_, writeErr = emitThreadEvent(ctx, f.firm, conv.ID, nil, types.EventMessageComplete,
			map[string]any{"message_id": messageID})
	}()

	// --- connection 1: read one delta, then hang up ---
	first, lastID := readFrames(t, srv.URL, conv.ID, cursor, "", 1)
	if deltasOf(first) == "" {
		t.Fatal("no delta arrived on the first connection")
	}

	// --- connection 2: resume with Last-Event-ID, read to completion ---
	second, _ := readFrames(t, srv.URL, conv.ID, 0, strconv.FormatUint(lastID, 10), 0)
	<-written
	if writeErr != nil {
		t.Fatalf("writing the reply: %v", writeErr)
	}

	if got, want := deltasOf(first)+deltasOf(second), strings.Join(parts, ""); got != want {
		t.Fatalf("replay does not reassemble the reply:\n  got  %q\n  want %q", got, want)
	}
	// The completion was delivered by REPLAY, not by the connection that died.
	if id := eventID(second, types.EventMessageComplete); id <= lastID {
		t.Fatalf("message.complete (id %d) is not after the severed cursor (%d)", id, lastID)
	}
	t.Logf("severed at %d, resumed and reassembled %d deltas", lastID, len(parts))
}

// --- helpers ----------------------------------------------------------------

// streamRouter mounts the two cursor endpoints with the auth/tenant context
// the middleware chain would have produced. It deliberately does not import
// controllers (that would be an import cycle) — the handlers are one line.
func streamRouter(f *convFixture) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middlewares.AuthUserKey, &u.AuthUser{ID: f.user, Email: f.user + "@example.test"})
		c.Set(u.TenantContextKey, f.firm)
		c.Next()
	})
	r.GET("/api/conversations/:id/stream", func(c *gin.Context) {
		var q types.EventsQuery
		_ = c.ShouldBindQuery(&q)
		ConversationStream(c, c.Param("id"), q)
	})
	r.GET("/api/conversations/:id/events", func(c *gin.Context) {
		var q types.EventsQuery
		_ = c.ShouldBindQuery(&q)
		c.JSON(ConversationEvents(c, c.Param("id"), q))
	})
	return r
}

type frame struct {
	id      uint64
	evType  string
	payload map[string]any
}

// readFrames opens one SSE connection and reads it. stopAfterDeltas > 0 kills
// the connection after that many delta frames — that is the "cable cut". With
// 0 it reads until message.complete.
func readFrames(t *testing.T, base, convID string, after uint64, lastEventID string, stopAfterDeltas int) ([]frame, uint64) {
	t.Helper()
	url := fmt.Sprintf("%s/api/conversations/%s/stream?after=%d", base, convID, after)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("stream connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q", ct)
	}
	for _, h := range [][2]string{
		{"Cache-Control", "no-cache, no-transform"},
		{"X-Accel-Buffering", "no"},
	} {
		if got := resp.Header.Get(h[0]); got != h[1] {
			t.Errorf("header %s = %q, want %q", h[0], got, h[1])
		}
	}

	var (
		frames  []frame
		cursor  = after
		deltas  int
		scanner = bufio.NewScanner(resp.Body)
		cur     frame
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.evType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var ev types.RunEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("frame payload: %v", err)
			}
			cur.payload = map[string]any{}
			_ = json.Unmarshal(ev.Payload, &cur.payload)
		case line == "":
			if cur.evType == "" {
				continue
			}
			if cur.id <= cursor && cursor != after {
				t.Errorf("frames out of order: %d after %d", cur.id, cursor)
			}
			cursor = cur.id
			frames = append(frames, cur)
			if cur.evType == types.EventMessageDelta {
				deltas++
			}
			done := (stopAfterDeltas > 0 && deltas >= stopAfterDeltas) ||
				(stopAfterDeltas == 0 && cur.evType == types.EventMessageComplete)
			cur = frame{}
			if done {
				return frames, cursor
			}
		}
	}
	return frames, cursor
}

// eventID is the id of the first frame of a given type, 0 when absent.
func eventID(frames []frame, evType string) uint64 {
	for _, f := range frames {
		if f.evType == evType {
			return f.id
		}
	}
	return 0
}

func deltasOf(frames []frame) string {
	var b strings.Builder
	for _, f := range frames {
		if f.evType != types.EventMessageDelta {
			continue
		}
		if s, ok := f.payload["delta"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func ref[T any](v T) *T { return &v }

// TestThreadPutsTheTurnBeforeItsAnswer: the agent intake writes a turn and
// its assistant placeholder in one transaction, so both rows share a
// created_at second and the ids alone decided which came first — a reload
// could show the answer above the question.
func TestThreadPutsTheTurnBeforeItsAnswer(t *testing.T) {
	f := newFixture(t)
	repo := u.ForTenantID(context.Background(), f.firm)

	conv := mustCreate(t, f, types.ConversationCreateRequest{})
	at := time.Now().Unix()
	for _, m := range []types.Message{
		// The answer's id sorts first.
		{ID: "0" + uuid.NewString()[1:], Role: types.MessageRoleAssistant, State: types.MessageStateStreaming},
		{ID: "f" + uuid.NewString()[1:], Role: types.MessageRoleUser, Content: "Résume ce dossier.", State: types.MessageStateComplete},
	} {
		m.ConversationID, m.CreatedAt, m.UpdatedAt = conv.ID, at, at
		if err := repo.Create(&m); err != nil {
			t.Fatalf("seed %s message: %v", m.Role, err)
		}
	}

	status, out := GetConversation(ctxFor(f.firm, f.user), conv.ID)
	if status != http.StatusOK {
		t.Fatalf("GetConversation: status %d body %v", status, out)
	}
	messages := out.(types.ConversationDetail).Messages
	if len(messages) != 2 || messages[0].Role != types.MessageRoleUser {
		t.Fatalf("thread order = %v, want the user's turn first", rolesOf(messages))
	}
}

func rolesOf(messages []types.Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, m.Role)
	}
	return out
}

// TestThreadResolvesTurnAttachments pins how a turn's attachments reach the
// client. The composer sends bare ids and the turn stores them as such; the
// thread used to serve them back as bare strings, which the client keyed on
// an undefined `id` — a thread whose first turn carried documents rendered
// blank. The thread now serves {id, kind, name}: pièces resolve within the
// tenant, and an id that resolves to nothing still comes back as an object.
func TestThreadResolvesTurnAttachments(t *testing.T) {
	f := newFixture(t)
	repo := u.ForTenantID(context.Background(), f.firm)

	var piece types.Piece
	if err := repo.Pieces().Where("dossier_id = ?", f.dossier).Order("ordinal").Take(&piece).Error; err != nil {
		t.Fatalf("load a seeded piece: %v", err)
	}

	conv := mustCreate(t, f, types.ConversationCreateRequest{})
	turn := types.Message{
		ID:             uuid.NewString(),
		ConversationID: conv.ID,
		Role:           types.MessageRoleUser,
		Content:        "Analyse ces pièces.",
		State:          types.MessageStateComplete,
		Attachments:    jsonbOfAny([]string{piece.ID, "missing"}),
	}
	if err := repo.Create(&turn); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	status, out := GetConversation(ctxFor(f.firm, f.user), conv.ID)
	if status != http.StatusOK {
		t.Fatalf("GetConversation: status %d body %v", status, out)
	}
	messages := out.(types.ConversationDetail).Messages
	if len(messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(messages))
	}

	var got []map[string]any
	if err := json.Unmarshal(messages[0].Attachments, &got); err != nil {
		t.Fatalf("attachments are not a list of objects: %s (%v)", messages[0].Attachments, err)
	}
	want := []map[string]any{
		{"id": piece.ID, "kind": "piece", "name": piece.OriginalName},
		{"id": "missing"},
	}
	if len(got) != len(want) {
		t.Fatalf("attachments = %v, want %v", got, want)
	}
	for i := range want {
		for key, value := range want[i] {
			if got[i][key] != value {
				t.Errorf("attachment %d: %s = %v, want %v (got %v)", i, key, got[i][key], value, got[i])
			}
		}
		if _, leaked := got[i]["name"]; leaked && want[i]["name"] == nil {
			t.Errorf("attachment %d carries a name it must not resolve: %v", i, got[i])
		}
	}
}
