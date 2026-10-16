package services

import (
	"bytes"
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

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Agent tools against a REAL Postgres.
//
// What these prove: every tool validates its arguments into an observation,
// private reads never cross the tenant, the dossier or the frozen scope,
// read_piece writes exactly the receipts and spans it delivered, the
// isolated web request carries nothing but the public query, and revisions
// are immutable with expected-hash edits.

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// toolBlob is an in-memory blob store (testsupport's cannot be imported from
// package services without a cycle).
type toolBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *toolBlob) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return nil
}

func (m *toolBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, u.ErrBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *toolBlob) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *toolBlob) SignedURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "mem://" + key, nil
}

func (m *toolBlob) Backend() string { return "mem" }

type toolFixture struct {
	t       *testing.T
	tenant  string
	user    string
	dossier types.Dossier
	runID   string
	blob    *toolBlob
	calls   int
}

func newToolFixture(t *testing.T) *toolFixture {
	t.Helper()
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres not reachable — agent tool suite skipped")
	}
	f := &toolFixture{
		t:      t,
		tenant: "test_firm_tools_" + uuid.NewString()[:8],
		user:   "test_user_tools_" + uuid.NewString()[:8],
		runID:  "run_tools_" + uuid.NewString(),
		blob:   &toolBlob{objects: map[string][]byte{}},
	}
	ctx := context.Background()
	if err := u.EnsureFirm(ctx, f.tenant, "Cabinet de test", f.user); err != nil {
		t.Fatalf("EnsureFirm: %v", err)
	}
	// Fixtures of one test share a store, so two tenants can be read side
	// by side.
	if shared, ok := u.Blob.(*toolBlob); ok {
		f.blob = shared
		t.Cleanup(func() { cleanupToolTenant(f.tenant, f.user) })
	} else {
		prev := u.Blob
		u.Blob = f.blob
		t.Cleanup(func() {
			u.Blob = prev
			cleanupToolTenant(f.tenant, f.user)
		})
	}
	f.dossier = f.newDossier("Vasseur c/ Transports Michelet")
	return f
}

func cleanupToolTenant(tenant, user string) {
	db := u.DB
	for _, table := range []string{
		"agent_read_receipts", "agent_evidence_spans", "agent_document_revisions", "agent_verifications",
		"agent_web_sources", "agent_turns", "llm_calls", "run_questions", "run_steps", "piece_chunks",
		"piece_parties", "pieces", "assets", "dossiers", "skill_versions", "skills", "audit_events",
	} {
		db.Exec(fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ?", table), tenant)
	}
	db.Exec("DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ? OR v0 = ?", tenant, tenant, user)
	db.Exec("DELETE FROM firms WHERE id = ?", tenant)
	if u.Enforcer != nil {
		_ = u.Enforcer.LoadPolicy()
	}
}

func (f *toolFixture) repo() *u.TenantRepo { return u.ForTenantID(context.Background(), f.tenant) }

func (f *toolFixture) newDossier(title string) types.Dossier {
	d := types.Dossier{ID: uuid.NewString(), Title: title, State: types.DossierStateActive, CreatedBy: f.user}
	if err := f.repo().Create(&d); err != nil {
		f.t.Fatalf("create dossier: %v", err)
	}
	return d
}

type pieceSpec struct {
	dossier  *types.Dossier
	ordinal  int
	name     string
	mime     string
	pages    []string // joined with form feeds into the sidecar; nil → no sidecar
	pageCnt  int
	state    string
	docDate  *int64
	datePrec string
}

const (
	mimePDF  = "application/pdf"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

func (f *toolFixture) addPiece(s pieceSpec) types.Piece {
	f.t.Helper()
	d := f.dossier
	if s.dossier != nil {
		d = *s.dossier
	}
	asset := types.Asset{
		ID: uuid.NewString(), S3Key: u.DossierKey(f.tenant, d.ID, uuid.NewString()+"-"+s.name),
		Kind: types.AssetKindOriginal, Mime: s.mime, Size: 10, SHA256: sha256Hex(uuid.NewString()),
	}
	state := firstNonEmpty(s.state, types.PieceStateProposed)
	p := types.Piece{
		ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: asset.ID, OriginalName: s.name,
		Ordinal: s.ordinal, ClassificationState: state, DatePrecision: firstNonEmpty(s.datePrec, types.DatePrecisionUnknown),
		PageCount: max(s.pageCnt, len(s.pages)), DocumentDate: s.docDate, Title: s.name,
	}
	if err := f.repo().Create(&asset); err != nil {
		f.t.Fatalf("create asset: %v", err)
	}
	if err := f.repo().Create(&p); err != nil {
		f.t.Fatalf("create piece: %v", err)
	}
	if s.pages != nil {
		f.blob.objects[textSidecarKey(f.tenant, d.ID, p.ID)] = []byte(strings.Join(s.pages, "\f"))
	}
	return p
}

// addChunk indexes text for a piece the way chunk_and_embed does, minus the
// embedding: the deny-list and the lexical arm read it.
func (f *toolFixture) addChunk(p types.Piece, ordinal int, text string) {
	f.t.Helper()
	if err := u.DB.Exec(`INSERT INTO piece_chunks (tenant_id, dossier_id, piece_id, ordinal, page_from, page_to, text, text_sha256, search_vector, created_at)
		VALUES (?, ?, ?, ?, 1, 1, ?, ?, to_tsvector('french', ?), 0)`, f.tenant, p.DossierID, p.ID, ordinal, text, sha256Hex(text), text).Error; err != nil {
		f.t.Fatalf("chunk: %v", err)
	}
}

func (f *toolFixture) contract() types.AgentTaskContract {
	return types.AgentTaskContract{
		Version: types.AgentTaskContractVersion, Instruction: "Chronologie des faits", OutputKinds: []string{types.AgentOutputChronology},
		Scope: types.AgentSourceScope{Mode: types.ScopeAllPieces, DossierID: f.dossier.ID}, Exhaustive: true,
	}
}

func (f *toolFixture) exec(contract types.AgentTaskContract) ToolExecution {
	f.calls++
	return ToolExecution{
		TenantID: f.tenant, ActorID: f.user, RunID: f.runID, StepID: "step_" + uuid.NewString()[:8],
		StepAttemptID: "att_" + uuid.NewString()[:8], ExecutionID: uuid.NewString(), Dispatch: f.calls,
		CallID: types.AgentCallID(f.calls, 0), Contract: contract,
		Permissions: ToolPermissions{DossierRead: true, DossierWrite: true},
		Budget:      ToolBudget{ReviewCallsLeft: types.MaxAgentReviewCalls, WebSearchesLeft: types.MaxAgentWebSearches},
	}
}

// call runs a tool with args (progress_note added) and persists its rows the
// way the runner's fenced transaction would.
func (f *toolFixture) call(x ToolExecution, tool string, args map[string]any) ToolOutcome {
	f.t.Helper()
	out, err := f.try(x, tool, args)
	if err != nil {
		f.t.Fatalf("%s: unexpected run error: %v", tool, err)
	}
	return out
}

func (f *toolFixture) try(x ToolExecution, tool string, args map[string]any) (ToolOutcome, error) {
	f.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args[types.ProgressNoteArg]; !ok {
		args[types.ProgressNoteArg] = "Je consulte le dossier."
	}
	raw, _ := json.Marshal(args)
	impl, ok := AgentTools.Lookup(tool)
	if !ok {
		f.t.Fatalf("tool %s not registered", tool)
	}
	out, err := impl.Execute(context.Background(), x, raw)
	if err != nil {
		return out, err
	}
	if out.Observation.CallID != x.CallID || out.Observation.Tool != tool {
		f.t.Fatalf("%s: observation envelope mismatch: %+v", tool, out.Observation)
	}
	if out.Persist != nil {
		if err := f.repo().Tx(out.Persist); err != nil {
			f.t.Fatalf("%s: persist: %v", tool, err)
		}
	}
	return out, nil
}

func obsData(t *testing.T, out ToolOutcome) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out.Observation.Data, &m); err != nil {
		t.Fatalf("observation data: %v", err)
	}
	return m
}

func wantCode(t *testing.T, out ToolOutcome, code string) {
	t.Helper()
	if out.Observation.Status != types.ObservationError || out.Observation.Code != code {
		t.Fatalf("want error %s, got status=%s code=%s data=%s", code, out.Observation.Status, out.Observation.Code, out.Observation.Data)
	}
}

func wantOK(t *testing.T, out ToolOutcome) {
	t.Helper()
	if out.Observation.Status == types.ObservationError {
		t.Fatalf("want success, got code=%s data=%s", out.Observation.Code, out.Observation.Data)
	}
}

// ---------------------------------------------------------------------------
// Definitions and argument decoding (no database)
// ---------------------------------------------------------------------------

func TestToolCatalogueIsStrictAndComplete(t *testing.T) {
	want := []string{
		types.ToolListPieces, types.ToolSearchPieces, types.ToolReadPiece, types.ToolSearchLaw, types.ToolReadLaw,
		types.ToolSearchCaselaw, types.ToolReadDecision, types.ToolSearchWeb, types.ToolReadEvidence,
		types.ToolListCompetences, types.ToolLoadCompetence, types.ToolAskLawyer, types.ToolWriteDocument,
		types.ToolEditDocument, types.ToolReadDocument, types.ToolCalculate, types.ToolVerifyOutput,
		types.ToolReviewOutput, types.ToolRenderDocument, types.ToolExportDossier, types.ToolSubmitResult,
	}
	for _, name := range want {
		tool, ok := AgentTools.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		def := tool.Definition()
		if err := ValidateToolDefinition(def); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertStrictSchema(t, name, def.Parameters)
		note := def.Parameters["properties"].(map[string]any)[types.ProgressNoteArg].(map[string]any)
		if note["maxLength"] != types.MaxProgressNoteRunes {
			t.Fatalf("%s: progress_note is not bounded to %d", name, types.MaxProgressNoteRunes)
		}
	}
	for _, name := range []string{types.ToolAskLawyer, types.ToolSubmitResult} {
		tool, _ := AgentTools.Lookup(name)
		if !tool.Definition().Exclusive {
			t.Fatalf("%s must be exclusive", name)
		}
	}
	if err := AgentTools.Register(listPiecesTool()); err == nil {
		t.Fatal("registering a name twice must fail")
	}
}

// assertStrictSchema walks a schema: every object closed, every property
// required (strict mode), recursively.
func assertStrictSchema(t *testing.T, where string, s map[string]any) {
	t.Helper()
	isObject := s["type"] == "object"
	if ts, ok := s["type"].([]any); ok {
		for _, v := range ts {
			if v == "object" {
				isObject = true
			}
		}
	}
	if isObject {
		if s["additionalProperties"] != false {
			t.Fatalf("%s: open object", where)
		}
		props, _ := s["properties"].(map[string]any)
		required := map[string]bool{}
		for _, r := range s["required"].([]any) {
			required[r.(string)] = true
		}
		for name, sub := range props {
			if !required[name] {
				t.Fatalf("%s.%s: property not required (strict mode)", where, name)
			}
			assertStrictSchema(t, where+"."+name, sub.(map[string]any))
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		assertStrictSchema(t, where+"[]", items)
	}
}

func TestDefinitionsHideUnavailableTools(t *testing.T) {
	prevVerify, prevArtifacts := VerifyOutput, AgentArtifacts
	VerifyOutput, AgentArtifacts = nil, nil
	t.Cleanup(func() { VerifyOutput, AgentArtifacts = prevVerify, prevArtifacts })
	for _, d := range AgentTools.Definitions() {
		switch d.Name {
		case types.ToolVerifyOutput, types.ToolRenderDocument, types.ToolExportDossier:
			t.Fatalf("%s offered while its Track C implementation is nil", d.Name)
		}
	}
	tool, _ := AgentTools.Lookup(types.ToolVerifyOutput)
	out, err := tool.Execute(context.Background(), ToolExecution{CallID: "d1.c0"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unavailable tool must answer with an observation: %v", err)
	}
	wantCode(t, out, types.ToolCodeToolUnavailable)
}

func TestDecodeArgsRejectsMalformedCalls(t *testing.T) {
	var a readPieceArgs
	long := strings.Repeat("é", types.MaxProgressNoteRunes+1)
	cases := map[string]string{
		"empty":         ``,
		"not an object": `[1,2]`,
		"unknown field": `{"progress_note":"x","piece_id":"p","tenant_id":"other"}`,
		"no note":       `{"piece_id":"p","cursor":null,"max_chars":null}`,
		"blank note":    `{"progress_note":"  ","piece_id":"p"}`,
		"long note":     `{"progress_note":"` + long + `","piece_id":"p"}`,
		"wrong type":    `{"progress_note":"x","piece_id":3}`,
		"trailing":      `{"progress_note":"x","piece_id":"p"} {}`,
	}
	for name, raw := range cases {
		err := decodeArgs(json.RawMessage(raw), &a)
		var ae *argError
		if !errors.As(err, &ae) {
			t.Fatalf("%s: want an argument error, got %v", name, err)
		}
	}
	if err := decodeArgs(json.RawMessage(`{"progress_note":"`+strings.Repeat("é", 140)+`","piece_id":"p","cursor":null,"max_chars":null}`), &a); err != nil {
		t.Fatalf("140-rune note must pass: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scope and tenancy
// ---------------------------------------------------------------------------

func TestPrivateToolsStayInsideTenantDossierAndScope(t *testing.T) {
	f := newToolFixture(t)
	inScope := f.addPiece(pieceSpec{ordinal: 1, name: "contrat.pdf", mime: mimePDF, pages: []string{"Contrat de travail signé le 3 mars 2020."}})
	excluded := f.addPiece(pieceSpec{ordinal: 2, name: "note.pdf", mime: mimePDF, pages: []string{"Note interne."}})

	other := f.newDossier("Autre dossier")
	foreignDossier := f.addPiece(pieceSpec{dossier: &other, ordinal: 1, name: "autre.pdf", mime: mimePDF, pages: []string{"Texte d'un autre dossier."}})

	g := newToolFixture(t)
	foreignTenant := g.addPiece(pieceSpec{ordinal: 1, name: "secret.pdf", mime: mimePDF, pages: []string{"Pièce d'un autre cabinet."}})

	c := f.contract()
	c.Scope.Excluded = []types.ScopeEntry{{PieceID: excluded.ID, Provenance: types.ContractProvenance{Source: types.ContractFromUserMessage}}}

	for name, id := range map[string]string{"foreign tenant": foreignTenant.ID, "other dossier": foreignDossier.ID, "excluded": excluded.ID, "made up": "nope"} {
		out := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": id, "cursor": nil, "max_chars": nil})
		wantCode(t, out, types.ToolCodeUnknownReference)
		if len(out.Observation.EvidenceIDs) != 0 || out.Persist != nil {
			t.Fatalf("%s: a refused read must persist nothing", name)
		}
	}
	out := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": inScope.ID, "cursor": nil, "max_chars": nil})
	wantOK(t, out)

	list := f.call(f.exec(c), types.ToolListPieces, map[string]any{"cursor": nil, "limit": nil})
	data := obsData(t, list)
	if data["total"].(float64) != 1 || data["excluded_count"].(float64) != 1 {
		t.Fatalf("inventory must list the scope only: %v", data)
	}

	// Explicit scope: exactly the inclusions; an explicit empty scope is empty.
	explicit := f.contract()
	explicit.Scope = types.AgentSourceScope{Mode: types.ScopeExplicit, DossierID: f.dossier.ID, Included: []types.ScopeEntry{}}
	list = f.call(f.exec(explicit), types.ToolListPieces, map[string]any{"cursor": nil, "limit": nil})
	if list.Observation.Status != types.ObservationEmpty {
		t.Fatalf("explicit empty scope must list nothing, got %s", list.Observation.Data)
	}

	// No dossier.
	none := f.contract()
	none.Scope = types.AgentSourceScope{Mode: types.ScopeNone}
	for tool, args := range map[string]map[string]any{
		types.ToolListPieces:   {"cursor": nil, "limit": nil},
		types.ToolReadPiece:    {"piece_id": inScope.ID, "cursor": nil, "max_chars": nil},
		types.ToolSearchPieces: {"query": "contrat", "piece_ids": nil, "limit": nil},
	} {
		out := f.call(f.exec(none), tool, args)
		wantCode(t, out, types.ToolCodeMissingDossier)
	}

	// Revoked read access fails the run instead of answering.
	x := f.exec(c)
	x.Permissions.DossierRead = false
	if _, err := f.try(x, types.ToolListPieces, map[string]any{"cursor": nil, "limit": nil}); !errors.Is(err, errDossierAccessRevoked) {
		t.Fatalf("revoked access must fail the run, got %v", err)
	}

	// A cross-tenant evidence id is unknown too.
	foreignOut := g.call(g.exec(g.contract()), types.ToolReadPiece, map[string]any{"piece_id": foreignTenant.ID, "cursor": nil, "max_chars": nil})
	wantOK(t, foreignOut)
	stolen := f.call(f.exec(c), types.ToolReadEvidence, map[string]any{"evidence_ids": foreignOut.Observation.EvidenceIDs})
	wantCode(t, stolen, types.ToolCodeUnknownReference)
}

func TestPieceToolsArgumentValidation(t *testing.T) {
	f := newToolFixture(t)
	p := f.addPiece(pieceSpec{ordinal: 1, name: "contrat.pdf", mime: mimePDF, pages: []string{"Texte."}})
	c := f.contract()
	cases := []struct {
		tool string
		args map[string]any
	}{
		{types.ToolListPieces, map[string]any{"cursor": nil, "limit": 0}},
		{types.ToolListPieces, map[string]any{"cursor": "o:-3", "limit": nil}},
		{types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": nil, "max_chars": 10}},
		{types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": "p99@0", "max_chars": nil}},
		{types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": "p1@500", "max_chars": nil}},
		{types.ToolSearchPieces, map[string]any{"query": "", "piece_ids": nil, "limit": nil}},
		{types.ToolSearchPieces, map[string]any{"query": "x", "piece_ids": nil, "limit": 99}},
		{types.ToolReadEvidence, map[string]any{"evidence_ids": []string{}}},
		{types.ToolLoadCompetence, map[string]any{"slug": "../etc"}},
		{types.ToolSearchLaw, map[string]any{"query": "licenciement", "applicability_date": "12/03/2025", "text_type": nil, "code": nil, "page": nil}},
		{types.ToolSearchCaselaw, map[string]any{"query": "faute grave", "cutoff_date": nil, "jurisdiction": nil, "chamber": nil, "published_only": nil, "page": 42}},
		{types.ToolReadLaw, map[string]any{"legal_version_id": nil, "legifrance_article_id": nil, "applicability_date": nil, "cursor": nil}},
		// No applicability date: never « today » by default.
		{types.ToolReadLaw, map[string]any{"legal_version_id": "v1", "legifrance_article_id": nil, "applicability_date": nil, "cursor": nil}},
		{types.ToolSearchLaw, map[string]any{"query": "licenciement", "applicability_date": nil, "text_type": nil, "code": nil, "page": nil}},
		// The code filter leaves the cabinet: a code's name, nothing else.
		{types.ToolSearchLaw, map[string]any{"query": "licenciement", "applicability_date": "2025-03-01", "text_type": nil, "code": "Brindille et associés", "page": nil}},
		{types.ToolReadDecision, map[string]any{"legal_version_id": "a", "judilibre_id": "b", "cutoff_date": nil, "cursor": nil}},
	}
	for _, tc := range cases {
		tool, _ := AgentTools.Lookup(tc.tool)
		if !tool.(availability).Available() {
			continue
		}
		out := f.call(f.exec(c), tc.tool, tc.args)
		wantCode(t, out, types.ToolCodeInvalidArguments)
	}
}

// ---------------------------------------------------------------------------
// read_evidence
// ---------------------------------------------------------------------------

func TestReadEvidenceRedeliversFrozenTextWithReceipts(t *testing.T) {
	f := newToolFixture(t)
	p := f.addPiece(pieceSpec{ordinal: 1, name: "lettre.pdf", mime: mimePDF, pages: []string{"Première page.", "Seconde page."}})
	c := f.contract()
	read := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": nil, "max_chars": nil})
	wantOK(t, read)
	if len(read.Observation.EvidenceIDs) != 2 || len(read.Observation.ReadReceiptIDs) != 2 {
		t.Fatalf("two pages, two spans and two receipts: %+v", read.Observation)
	}

	receiptsOf := func(x ToolExecution) []types.AgentReadReceipt {
		var receipts []types.AgentReadReceipt
		f.repo().AgentReadReceipts().Where("run_id = ? AND tool_call_id = ?", f.runID, x.CallID).Find(&receipts)
		return receipts
	}
	var first types.AgentReadReceipt
	f.repo().AgentReadReceipts().Where("id = ?", read.Observation.ReadReceiptIDs[0]).Take(&first)

	// Re-reading unchanged text proposes one receipt per pièce span, pinned
	// to the same extraction as the first reading.
	x := f.exec(c)
	wantOK(t, f.call(x, types.ToolReadEvidence, map[string]any{"evidence_ids": read.Observation.EvidenceIDs}))
	if rs := receiptsOf(x); len(rs) != 2 || rs[0].ExtractionVersion != first.ExtractionVersion || rs[0].SourceSHA256 != first.SourceSHA256 {
		t.Fatalf("read_evidence must propose one receipt per re-delivered pièce span: %+v", rs)
	}

	// The sidecar changes afterwards: re-reading the evidence returns the
	// FROZEN quote, and proposes no receipt — the old passage proves nothing
	// about today's text.
	f.blob.objects[textSidecarKey(f.tenant, f.dossier.ID, p.ID)] = []byte("Texte modifié.\fAutre.")
	x = f.exec(c)
	again := f.call(x, types.ToolReadEvidence, map[string]any{"evidence_ids": read.Observation.EvidenceIDs})
	wantOK(t, again)
	items := obsData(t, again)["evidence"].([]any)
	if items[0].(map[string]any)["quote"] != "Première page." {
		t.Fatalf("frozen quote expected, got %v", items[0])
	}
	if rs := receiptsOf(x); len(rs) != 0 {
		t.Fatalf("a changed piece must not get receipts from frozen text, got %d", len(rs))
	}
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

func TestWriteEditReadDocumentRevisions(t *testing.T) {
	f := newToolFixture(t)
	p := f.addPiece(pieceSpec{ordinal: 1, name: "contrat.pdf", mime: mimePDF, pages: []string{"Le contrat est signé le 3 mars 2020 pour un salaire de 2 450,75 euros."}})
	c := f.contract()
	read := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": nil, "max_chars": nil})
	ev := read.Observation.EvidenceIDs[0]

	block := func(id any, text, claim string, evidence []string) map[string]any {
		return map[string]any{
			"id": id, "role": types.BlockChronologyRow, "level": nil, "text": text, "bordereau": nil,
			"chronology_date": map[string]any{"precision": "day", "year": 2020, "month": 3, "day": 3, "role": "event"},
			"claims": []any{map[string]any{
				"id": nil, "text": claim, "occurrence": nil, "kind": types.ClaimFactual, "evidence_ids": evidence,
				"answer_keys": nil, "calculation_call_id": nil, "amounts": nil, "quotes": nil,
				"dates": []any{map[string]any{"precision": "day", "year": 2020, "month": 3, "day": 3, "role": "event"}},
			}},
		}
	}
	write := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "chronologie", "kind": types.AgentOutputChronology, "title": "Chronologie", "skill_version_id": nil,
		"blocks": []any{block(nil, "3 mars 2020 — signature du contrat de travail.", "signature du contrat", []string{ev})},
	})
	wantOK(t, write)
	w := obsData(t, write)
	rev1, sha1 := w["revision_id"].(string), w["content_sha256"].(string)

	var stored types.AgentDocumentRevision
	if err := f.repo().AgentDocumentRevisions().Where("id = ?", rev1).Take(&stored).Error; err != nil {
		t.Fatalf("revision not persisted: %v", err)
	}
	body, _ := revisionBody(stored)
	claim := body.Blocks[0].Claims[0]
	if body.Blocks[0].ID != "b1" || claim.ID != "b1.c1" || claim.Start != 14 || claim.End != 34 {
		t.Fatalf("server ids and code-point offsets expected, got block=%s claim=%+v", body.Blocks[0].ID, claim)
	}
	if want, _ := types.DocumentContentSHA256(stored.Kind, stored.Title, body); want != stored.ContentSHA256 || want != sha1 {
		t.Fatal("content hash must be DocumentContentSHA256 of the stored body")
	}

	// Same key again → conflict pointing at the latest revision.
	again := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "chronologie", "kind": types.AgentOutputChronology, "title": "Chronologie", "skill_version_id": nil,
		"blocks": []any{block(nil, "Autre.", "Autre", nil)},
	})
	wantCode(t, again, types.ToolCodeRevisionConflict)

	// Unknown evidence and a claim text absent from its block are refused.
	bad := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "autre", "kind": types.AgentOutputAnswer, "title": "Réponse", "skill_version_id": nil,
		"blocks": []any{block(nil, "Texte.", "Texte", []string{"ev_inexistant"})},
	})
	wantCode(t, bad, types.ToolCodeUnknownReference)
	absent := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "autre", "kind": types.AgentOutputAnswer, "title": "Réponse", "skill_version_id": nil,
		"blocks": []any{block(nil, "Texte.", "pas dans le bloc", nil)},
	})
	wantCode(t, absent, types.ToolCodeInvalidArguments)

	edit := func(rev, sha string) ToolOutcome {
		return f.call(f.exec(c), types.ToolEditDocument, map[string]any{
			"revision_id": rev, "expected_sha256": sha, "title": nil,
			"edits": []any{map[string]any{"op": "append", "block_id": nil, "block": map[string]any{
				"id": nil, "role": types.BlockLimitation, "level": nil, "text": "Pièces 2 à 18 non lues.", "claims": nil,
				"chronology_date": nil, "bordereau": nil,
			}}},
		})
	}
	wantCode(t, edit(rev1, strings.Repeat("0", 64)), types.ToolCodeRevisionConflict)
	ok := edit(rev1, sha1)
	wantOK(t, ok)
	rev2 := obsData(t, ok)["revision_id"].(string)
	if obsData(t, ok)["revision"].(float64) != 2 {
		t.Fatalf("edit must create revision 2: %s", ok.Observation.Data)
	}
	// Editing the superseded revision is stale even with its right hash.
	wantCode(t, edit(rev1, sha1), types.ToolCodeRevisionConflict)

	readDoc := f.call(f.exec(c), types.ToolReadDocument, map[string]any{"revision_id": nil, "document_key": "chronologie", "cursor": nil})
	d := obsData(t, readDoc)
	if d["revision_id"] != rev2 || d["is_latest"] != true || len(d["blocks"].([]any)) != 2 {
		t.Fatalf("read_document by key must return the latest revision: %v", d)
	}

	// Another run cannot see these revisions.
	other := f.exec(c)
	other.RunID = "run_tools_other_" + uuid.NewString()
	raw, _ := json.Marshal(map[string]any{"progress_note": "x", "revision_id": rev2, "document_key": nil, "cursor": nil})
	tool, _ := AgentTools.Lookup(types.ToolReadDocument)
	foreign, err := tool.Execute(context.Background(), other, raw)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, foreign, types.ToolCodeUnknownReference)
}

// A revision is reviewed again only after a major finding: once the latest
// review of a document raised nothing major, review_output skips it instead
// of calling the reviewer (production looped verify → review → edit until
// the budget ran out).
func TestReviewOutputSkipsADocumentWithoutMajorFindings(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	limitation := func(text string) map[string]any {
		return map[string]any{"id": nil, "role": types.BlockLimitation, "level": nil, "text": text, "claims": nil,
			"chronology_date": nil, "bordereau": nil}
	}
	write := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "reponse", "kind": types.AgentOutputAnswer, "title": "Réponse", "skill_version_id": nil,
		"blocks": []any{limitation("Pièces non lues.")},
	})
	wantOK(t, write)
	edit := func(prev ToolOutcome) ToolOutcome {
		d := obsData(t, prev)
		out := f.call(f.exec(c), types.ToolEditDocument, map[string]any{
			"revision_id": d["revision_id"], "expected_sha256": d["content_sha256"], "title": nil,
			"edits": []any{map[string]any{"op": "append", "block_id": nil, "block": limitation("Précision.")}},
		})
		wantOK(t, out)
		return out
	}
	reviewed := func(rev ToolOutcome, severities ...string) {
		findings := []any{}
		for _, s := range severities {
			findings = append(findings, map[string]any{"severity": s})
		}
		output, _ := json.Marshal(map[string]any{"observation": map[string]any{
			"tool": types.ToolReviewOutput, "status": types.ObservationOK,
			"data": map[string]any{"revision_id": obsData(t, rev)["revision_id"], "findings": findings},
		}})
		step := types.RunStep{ID: uuid.NewString(), RunID: f.runID, StepKey: "agent:tool:" + uuid.NewString()[:8],
			Kind: types.StepKindLLM, State: types.StepStateSucceeded, AvailableAt: time.Now(),
			IdempotencyKey: uuid.NewString(), Output: types.JSONB(output)}
		if err := f.repo().Create(&step); err != nil {
			t.Fatal(err)
		}
	}
	majors := func() (int, bool) {
		n, ok, err := lastReviewMajors(context.Background(), f.exec(c), "reponse")
		if err != nil {
			t.Fatal(err)
		}
		return n, ok
	}

	if _, ok := majors(); ok {
		t.Fatal("a document never reviewed has no last review")
	}
	reviewed(write, "major", "minor")
	if n, ok := majors(); !ok || n != 1 {
		t.Fatalf("want 1 major on the last review, got %d (reviewed=%t)", n, ok)
	}

	// The major was fixed in revision 2, whose review found only a nit:
	// revision 3 is not sent to the reviewer.
	rev2 := edit(write)
	reviewed(rev2, "minor")
	rev3 := edit(rev2)
	raw, _ := json.Marshal(map[string]any{"progress_note": "Je fais relire.", "revision_id": obsData(t, rev3)["revision_id"], "focus": nil})
	out, err := execReviewOutput(context.Background(), f.exec(c), raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Observation.Status != types.ObservationEmpty || obsData(t, out)["skipped"] != true {
		t.Fatalf("want a skipped review, got status=%s data=%s", out.Observation.Status, out.Observation.Data)
	}
}

// ---------------------------------------------------------------------------
// calculate
// ---------------------------------------------------------------------------

func TestCalculateIsExactDecimal(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	calc := func(expr string, operands []any, extra map[string]any) ToolOutcome {
		args := map[string]any{"mode": "expression", "expression": expr, "operands": operands, "scale": nil, "rounding": nil, "from_date": nil, "to_date": nil}
		for k, v := range extra {
			args[k] = v
		}
		return f.call(f.exec(c), types.ToolCalculate, args)
	}
	op := func(name, value string, unit any) map[string]any {
		return map[string]any{"name": name, "value": value, "unit": unit, "evidence_ids": nil}
	}

	out := calc("a + b", []any{op("a", "0.1", nil), op("b", "0.2", nil)}, nil)
	if d := obsData(t, out); d["result"] != "0.30" || d["rounded"] != false {
		t.Fatalf("0.1 + 0.2 must be exactly 0.30: %v", d)
	}
	out = calc("salaire * 3 / 4", []any{op("salaire", "2 450,75", "EUR")}, nil)
	d := obsData(t, out)
	money := d["money"].(map[string]any)
	if d["result"] != "1838.06" || money["minor_units"].(float64) != 183806 || money["currency"] != "EUR" || d["rounded"] != true {
		t.Fatalf("money must round half-up to the cent in minor units: %v", d)
	}
	out = calc("x / 2", []any{op("x", "0.125", nil)}, map[string]any{"rounding": "half_even", "scale": 3})
	if obsData(t, out)["result"] != "0.062" {
		t.Fatalf("half-even rounding: %s", out.Observation.Data)
	}
	for _, tc := range []struct {
		expr string
		ops  []any
	}{
		{"a + b", []any{op("a", "1", "EUR"), op("b", "1", nil)}},       // units differ
		{"a * b", []any{op("a", "1", "EUR"), op("b", "2", "EUR")}},     // amount × amount
		{"a / 0", []any{op("a", "1", nil)}},                            // division by zero
		{"a + ", []any{op("a", "1", nil)}},                             // incomplete
		{"a + zz", []any{op("a", "1", nil)}},                           // unknown operand
		{"a", []any{op("a", "1e5", nil)}},                              // not a decimal
		{"(a + 1", []any{op("a", "1", nil)}},                           // unbalanced
		{"a + b", []any{op("a", "1,5", nil), op("b", "1.234,5", nil)}}, // ambiguous separator
	} {
		wantCode(t, calc(tc.expr, tc.ops, nil), types.ToolCodeInvalidArguments)
	}
	unknown := calc("a", []any{map[string]any{"name": "a", "value": "1", "unit": nil, "evidence_ids": []string{"ev_nope"}}}, nil)
	wantCode(t, unknown, types.ToolCodeUnknownReference)

	diff := f.call(f.exec(c), types.ToolCalculate, map[string]any{
		"mode": "date_difference", "expression": nil, "operands": nil, "scale": nil, "rounding": nil,
		"from_date": "2019-03-15", "to_date": "2025-01-10",
	})
	dd := obsData(t, diff)
	if dd["days"].(float64) != 2128 || dd["years"].(float64) != 5 || dd["months"].(float64) != 9 || dd["remaining_days"].(float64) != 26 {
		t.Fatalf("date difference: %v", dd)
	}
}

// Owner decision 5: write_document refuses an acte the lawyer did not put
// under a compétence, whatever skill version the agent names.
func TestWriteDocumentRefusesAnActeWithoutTheLawyersCompetence(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	blocks := []any{map[string]any{"id": nil, "role": types.BlockParagraph, "level": nil, "text": "Projet de requête.", "claims": nil, "chronology_date": nil, "bordereau": nil}}
	out := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "requete", "kind": types.AgentOutputActe, "title": "Requête", "skill_version_id": nil, "blocks": blocks,
	})
	wantCode(t, out, types.ToolCodeInvalidArguments)
	if !strings.Contains(string(out.Observation.Data), "compétence") || out.Persist != nil {
		t.Fatalf("the refusal must say why and write nothing: %s", out.Observation.Data)
	}
}

// ---------------------------------------------------------------------------
// ask_lawyer and submit_result
// ---------------------------------------------------------------------------

func TestControlToolsValidateThenHandOverToTheRuntime(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	c.RequiredDecisions = []types.RequiredDecision{{Key: "periode", Description: "Période retenue", State: types.DecisionOpen}}
	question := func(key string, decision any) map[string]any {
		return map[string]any{"key": key, "prompt": "Quelle période retenir ?", "kind": types.QuestionKindSingle,
			"options":  []any{map[string]any{"id": "a", "label": "Du contrat au solde"}, map[string]any{"id": "b", "label": "Toute la relation"}},
			"required": true, "decision_key": decision}
	}
	bad := f.call(f.exec(c), types.ToolAskLawyer, map[string]any{"questions": []any{question("periode", "inconnue")}})
	wantCode(t, bad, types.ToolCodeInvalidArguments)
	dup := f.call(f.exec(c), types.ToolAskLawyer, map[string]any{"questions": []any{question("q1", nil), question("q1", nil)}})
	wantCode(t, dup, types.ToolCodeInvalidArguments)
	asked := f.call(f.exec(c), types.ToolAskLawyer, map[string]any{"questions": []any{question("q1", "periode")}})
	if asked.Observation.Status != types.ObservationOK {
		t.Fatalf("a valid ask_lawyer must hand its questions to the runtime: %+v", asked.Observation)
	}
	var set agentAskQuestions
	if err := json.Unmarshal(asked.Observation.Data, &set); err != nil || len(set.Questions) != 1 ||
		len(set.Questions[0].Options) != 2 || set.Questions[0].Options[0].Value != "a" || !set.Questions[0].Required {
		t.Fatalf("questions must reach the runtime in its shape: %s", asked.Observation.Data)
	}
	if qs, problem := agentParseQuestions(asked.Observation, 1); problem != "" || len(qs) != 1 {
		t.Fatalf("the runtime must accept the tool's questions: %q", problem)
	}
	// A compétence is offered by its slug, hyphens included (owner decision 5).
	offer := f.call(f.exec(c), types.ToolAskLawyer, map[string]any{"questions": []any{map[string]any{
		"key": "competence", "prompt": "Préparer la requête sous cette compétence ?", "kind": types.QuestionKindSingle,
		"options": []any{
			map[string]any{"id": "requete-cph-resiliation-judiciaire", "label": "Requête CPH — résiliation judiciaire"},
			map[string]any{"id": "aucune", "label": "Aucune"},
		},
		"required": true, "decision_key": nil,
	}}})
	wantOK(t, offer)
	bogus := f.call(f.exec(c), types.ToolAskLawyer, map[string]any{"questions": []any{map[string]any{
		"key": "competence", "prompt": "Laquelle ?", "kind": types.QuestionKindSingle,
		"options":  []any{map[string]any{"id": "Pas Un Slug", "label": "A"}, map[string]any{"id": "b", "label": "B"}},
		"required": true, "decision_key": nil,
	}}})
	wantCode(t, bogus, types.ToolCodeInvalidArguments)

	partial := f.call(f.exec(c), types.ToolSubmitResult, map[string]any{
		"outcome": types.AgentOutcomePartial, "answer_revision_id": "rev_x", "document_revision_ids": nil, "limitations": nil, "thread_title": nil,
	})
	wantCode(t, partial, types.ToolCodeInvalidArguments)
	unknown := f.call(f.exec(c), types.ToolSubmitResult, map[string]any{
		"outcome": types.AgentOutcomeCompleted, "answer_revision_id": "rev_x", "document_revision_ids": nil, "limitations": nil, "thread_title": nil,
	})
	wantCode(t, unknown, types.ToolCodeUnknownReference)

	write := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "reponse", "kind": types.AgentOutputAnswer, "title": "Réponse", "skill_version_id": nil,
		"blocks": []any{map[string]any{"id": nil, "role": types.BlockParagraph, "level": nil, "text": "Bonjour.", "claims": nil, "chronology_date": nil, "bordereau": nil}},
	})
	rev := obsData(t, write)["revision_id"].(string)
	submitted := f.call(f.exec(c), types.ToolSubmitResult, map[string]any{
		"outcome": types.AgentOutcomeCompleted, "answer_revision_id": rev, "document_revision_ids": nil, "limitations": nil, "thread_title": nil,
	})
	var sub agentSubmissionData
	if submitted.Observation.Status != types.ObservationOK || json.Unmarshal(submitted.Observation.Data, &sub) != nil ||
		sub.AnswerRevisionID != rev || sub.Outcome != types.AgentOutcomeCompleted {
		t.Fatalf("a valid submission must name its answer for the final gate: %+v", submitted.Observation)
	}
}

// ---------------------------------------------------------------------------
// verify_output, render_document — through Track C's interfaces
// ---------------------------------------------------------------------------

type fakePreparer struct{ calls int }

func (p *fakePreparer) PrepareArtifact(_ context.Context, rev VerifiedRevision, _ ArtifactFormat) (StagedArtifact, error) {
	p.calls++
	id := rev.Revision.ID
	return StagedArtifact{Kind: types.ArtifactKindNoteDocx, Name: rev.Revision.Title + ".docx", SHA256: rev.Revision.ContentSHA256, Size: 1, DocumentRevisionID: &id}, nil
}

func TestVerifyThenRenderGoThroughTrackC(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	write := f.call(f.exec(c), types.ToolWriteDocument, map[string]any{
		"document_key": "reponse", "kind": types.AgentOutputAnswer, "title": "Réponse", "skill_version_id": nil,
		"blocks": []any{map[string]any{"id": nil, "role": types.BlockParagraph, "level": nil, "text": "Bonjour.", "claims": nil, "chronology_date": nil, "bordereau": nil}},
	})
	rev := obsData(t, write)["revision_id"].(string)

	prevVerify, prevArtifacts := VerifyOutput, AgentArtifacts
	t.Cleanup(func() { VerifyOutput, AgentArtifacts = prevVerify, prevArtifacts })
	prep := &fakePreparer{}
	AgentArtifacts = prep
	result := types.VerificationBlocked
	VerifyOutput = func(_ context.Context, in VerificationInput) (types.VerificationReport, error) {
		var r types.AgentDocumentRevision
		if err := u.ForTenantID(context.Background(), in.TenantID).AgentDocumentRevisions().Where("id = ?", in.DocumentRevisionID).Take(&r).Error; err != nil {
			return types.VerificationReport{}, err
		}
		return types.VerificationReport{VerifierVersion: "test-" + result, DocumentRevisionID: r.ID, ContentSHA256: r.ContentSHA256,
			ScopeSHA256: "s", EvidenceSHA256: "e", Result: result, DownloadAllowed: result != types.VerificationBlocked,
			Blocking: map[bool]int{true: 1}[result == types.VerificationBlocked]}, nil
	}

	// Not verified yet → refused.
	render := func() ToolOutcome {
		return f.call(f.exec(c), types.ToolRenderDocument, map[string]any{"revision_id": rev, "format": "docx"})
	}
	wantCode(t, render(), types.ToolCodeVerificationRefused)

	ver := f.call(f.exec(c), types.ToolVerifyOutput, map[string]any{"revision_id": rev})
	if obsData(t, ver)["result"] != types.VerificationBlocked || *ver.Counts.Blocking != 1 {
		t.Fatalf("verification observation: %s", ver.Observation.Data)
	}
	wantCode(t, render(), types.ToolCodeVerificationRefused)

	// The passing verification is the newer one (created_at is in seconds).
	u.DB.Exec("UPDATE agent_verifications SET created_at = created_at - 10 WHERE tenant_id = ?", f.tenant)
	result = types.VerificationPassed
	f.call(f.exec(c), types.ToolVerifyOutput, map[string]any{"revision_id": rev})
	if n := countRows(t, f, "agent_verifications"); n != 2 {
		t.Fatalf("each verification is stored by the caller, want 2 rows, got %d", n)
	}
	out := render()
	wantOK(t, out)
	if prep.calls != 1 {
		t.Fatalf("PrepareArtifact must be called once, got %d", prep.calls)
	}
}

func countRows(t *testing.T, f *toolFixture, table string) int64 {
	t.Helper()
	var n int64
	if err := u.DB.Table(table).Where("tenant_id = ?", f.tenant).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// search_web — the isolated request, against a fake OpenRouter
// ---------------------------------------------------------------------------

func TestSearchWebSendsOnlyThePublicQuery(t *testing.T) {
	f := newToolFixture(t)
	contract := f.addPiece(pieceSpec{ordinal: 1, name: "contrat.pdf", mime: mimePDF, pages: []string{"Monsieur Vasseur a été engagé par la société le 3 mars 2020 en qualité de chauffeur."}})
	f.addChunk(contract, 0, "Monsieur Vasseur a été engagé par la société le 3 mars 2020 en qualité de chauffeur.")
	turn := types.AgentTurn{ID: uuid.NewString(), RunID: f.runID, ActorID: f.user, RequestID: uuid.NewString(), RequestSHA256: "x",
		MaxDispatches: types.MaxAgentDispatches, DeadlineAt: time.Now().Add(time.Hour).Unix(), ControllerModel: "m",
		PromptVersion: "p", ToolsetVersion: "t", TaskContract: types.JSONB(`{}`)}
	if err := f.repo().Create(&turn); err != nil {
		t.Fatalf("agent turn: %v", err)
	}

	var hits atomic.Int32
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"id":"gen-1","model":"openai/gpt-6-luna","choices":[{"finish_reason":"stop","message":{
			"content":"Les sources indiquent que la faute grave prive du préavis.",
			"annotations":[
			 {"type":"url_citation","url_citation":{"url":"https://www.service-public.fr/particuliers/vosdroits/F2835#x","title":"Faute grave","content":"La faute grave prive le salarié du préavis."}},
			 {"type":"url_citation","url_citation":{"url":"https://example.org/lead","title":"Piste","content":""}},
			 {"type":"url_citation","url_citation":{"url":"javascript:alert(1)","title":"x","content":"y"}}]}}],
			"usage":{"prompt_tokens":120,"completion_tokens":40,"cost":0.0071,"server_tool_use_details":{"web_search_requests":1}}}`)
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("DRAFTER_MODEL", "openai/gpt-6-luna:online")

	c := f.contract()
	query := "faute grave privation du préavis jurisprudence"
	out := f.call(f.exec(c), types.ToolSearchWeb, map[string]any{"query": query})
	wantOK(t, out)

	// The payload: the drafter without :online, one Perplexity search, the
	// query and a static prompt — nothing else.
	if body["model"] != "openai/gpt-6-luna" || body["max_tool_calls"].(float64) != 1 {
		t.Fatalf("model/budget: %v %v", body["model"], body["max_tool_calls"])
	}
	tools := body["tools"].([]any)
	params := tools[0].(map[string]any)["parameters"].(map[string]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "openrouter:web_search" || params["engine"] != "perplexity" ||
		params["max_uses"].(float64) != 1 || params["max_results"].(float64) != webSearchMaxResults {
		t.Fatalf("server tool: %v", tools)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "<requete>"+query+"</requete>" ||
		msgs[0].(map[string]any)["content"] != webSearchToolPrompt {
		t.Fatalf("messages must be the static prompt and the query only: %v", msgs)
	}
	raw, _ := json.Marshal(body)
	for _, private := range []string{"Vasseur", "Michelet", f.dossier.ID, f.tenant} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private context %q leaked into the web request", private)
		}
	}
	if body["provider"].(map[string]any)["data_collection"] != "deny" {
		t.Fatal("data collection must be denied")
	}

	// Sources: two kept (the javascript: URL is dropped), one citable.
	results := obsData(t, out)["results"].([]any)
	if len(results) != 2 || len(out.Observation.EvidenceIDs) != 1 {
		t.Fatalf("want 2 sources and 1 citable excerpt: %s", out.Observation.Data)
	}
	if results[0].(map[string]any)["url"] != "https://www.service-public.fr/particuliers/vosdroits/F2835" {
		t.Fatalf("URL must be canonical, fragment dropped: %v", results[0])
	}
	if n := countRows(t, f, "agent_web_sources"); n != 2 {
		t.Fatalf("web sources persisted: %d", n)
	}
	var call types.LLMCall
	if err := f.repo().LLMCalls().Where("run_id = ? AND purpose = ?", f.runID, types.LLMPurposeWebSearch).Take(&call).Error; err != nil {
		t.Fatalf("llm_call: %v", err)
	}
	if call.State != types.LLMCallStateSucceeded || call.UsageStatus != types.UsageStatusKnown || call.ReturnedModel != "openai/gpt-6-luna" ||
		!strings.Contains(string(call.UsageDetails), "web_search_requests") {
		t.Fatalf("accounting: %+v", call)
	}
	if call.CostNanoUSD != 7_100_000 {
		t.Fatalf("cost = %d nano-USD, want the provider's 0.0071 USD", call.CostNanoUSD)
	}
	var turnNow types.AgentTurn
	f.repo().AgentTurns().Where("run_id = ?", f.runID).Take(&turnNow)
	if turnNow.WebSearches != 1 {
		t.Fatalf("the web budget slot must be reserved, got %d", turnNow.WebSearches)
	}

	// A query carrying dossier facts never reaches the network.
	before := hits.Load()
	for _, q := range []string{"licenciement Vasseur faute grave", "préavis contrat du 3 mars 2020", "écrire à jean@exemple.fr"} {
		refused := f.call(f.exec(c), types.ToolSearchWeb, map[string]any{"query": q})
		wantCode(t, refused, types.ToolCodeUnsafePublicQuery)
	}
	// Without a dossier, the names and dates the lawyer typed are denied too.
	bare := types.AgentTaskContract{Version: types.AgentTaskContractVersion, Scope: types.AgentSourceScope{Mode: types.ScopeNone},
		Instruction: "M. Kowalczyk, licencié par Brindille le 14 mars 2025, conteste la rupture."}
	for _, q := range []string{"licenciement Kowalczyk transporteur", "Brindille faute grave", "licenciement du 14 mars 2025"} {
		wantCode(t, f.call(f.exec(bare), types.ToolSearchWeb, map[string]any{"query": q}), types.ToolCodeUnsafePublicQuery)
	}
	if hits.Load() != before {
		t.Fatal("a refused query was sent")
	}

	// Budget: an exhausted budget refuses before any call.
	x := f.exec(c)
	x.Budget.WebSearchesLeft = 0
	wantCode(t, f.call(x, types.ToolSearchWeb, map[string]any{"query": query}), types.ToolCodeBudgetExhausted)
	u.DB.Model(&types.AgentTurn{}).Where("run_id = ?", f.runID).Update("web_searches", types.MaxAgentWebSearches)
	wantCode(t, f.call(f.exec(c), types.ToolSearchWeb, map[string]any{"query": query}), types.ToolCodeBudgetExhausted)
	if hits.Load() != before {
		t.Fatal("an over-budget query was sent")
	}
}

func TestPublicResearchToolsApplyTheDenyList(t *testing.T) {
	f := newToolFixture(t)
	c := f.contract()
	// The dossier title names the client: a law or case-law query naming
	// him is refused before any connector is asked.
	for tool, args := range map[string]map[string]any{
		types.ToolSearchLaw:     {"query": "licenciement de Vasseur", "applicability_date": "2025-03-01", "text_type": nil, "code": nil, "page": nil},
		types.ToolSearchCaselaw: {"query": "licenciement de Vasseur", "cutoff_date": nil, "jurisdiction": nil, "chamber": nil, "published_only": nil, "page": nil},
	} {
		impl, _ := AgentTools.Lookup(tool)
		if !impl.(availability).Available() {
			continue
		}
		out := f.call(f.exec(c), tool, args)
		wantCode(t, out, types.ToolCodeUnsafePublicQuery)
		if obsData(t, out)["category"] != PublicQueryDossierTitle {
			t.Fatalf("%s: category: %s", tool, out.Observation.Data)
		}
	}
	// search_law's code filter goes through the same gate as its query.
	if impl, _ := AgentTools.Lookup(types.ToolSearchLaw); impl.(availability).Available() {
		out := f.call(f.exec(c), types.ToolSearchLaw, map[string]any{
			"query": "licenciement faute grave", "applicability_date": "2025-03-01", "text_type": nil, "code": "Code Vasseur", "page": nil})
		wantCode(t, out, types.ToolCodeUnsafePublicQuery)
	}
}

// ---------------------------------------------------------------------------
// Live smoke — Légifrance and Judilibre, only with PISTE credentials
// ---------------------------------------------------------------------------

func TestLiveLegalResearchSmoke(t *testing.T) {
	testmode.Live(t, "Légifrance/Judilibre smoke (live PISTE)")
	f := newToolFixture(t)
	if !legifranceReady() || !judilibreReady() {
		testmode.Missing(t, "no PISTE credentials — live Légifrance/Judilibre smoke skipped")
	}
	c := f.contract()
	c.Scope = types.AgentSourceScope{Mode: types.ScopeNone}

	first := func(out ToolOutcome, key string) string {
		for _, cand := range obsData(t, out)["candidates"].([]any) {
			if id, _ := cand.(map[string]any)[key].(string); id != "" {
				return id
			}
		}
		t.Fatalf("no candidate with %s: %s", key, out.Observation.Data)
		return ""
	}
	search := f.call(f.exec(c), types.ToolSearchLaw, map[string]any{
		"query": "licenciement faute grave préavis", "applicability_date": "2024-06-01", "text_type": nil, "code": "Code du travail", "page": nil,
	})
	wantOK(t, search)
	law := f.call(f.exec(c), types.ToolReadLaw, map[string]any{
		"legal_version_id": nil, "legifrance_article_id": first(search, "legifrance_article_id"), "applicability_date": "2024-06-01", "cursor": nil,
	})
	wantOK(t, law)
	if len(law.Observation.EvidenceIDs) == 0 || obsData(t, law)["legal_version_id"] == "" {
		t.Fatalf("read_law must return a citable span on a stored version: %s", law.Observation.Data)
	}

	cases := f.call(f.exec(c), types.ToolSearchCaselaw, map[string]any{
		"query": "faute grave préavis", "cutoff_date": "2024-12-31", "jurisdiction": "cc", "chamber": "soc", "published_only": true, "page": nil,
	})
	wantOK(t, cases)
	read := f.call(f.exec(c), types.ToolReadDecision, map[string]any{
		"legal_version_id": nil, "judilibre_id": first(cases, "judilibre_id"), "cutoff_date": "2024-12-31", "cursor": nil,
	})
	wantOK(t, read)
	if len(read.Observation.EvidenceIDs) == 0 || obsData(t, read)["after_cutoff"] == true {
		t.Fatalf("read_decision must return paragraph spans within the cutoff: %s", read.Observation.Data)
	}
	t.Logf("live: article spans=%d decision paragraphs=%d", len(law.Observation.EvidenceIDs), len(read.Observation.EvidenceIDs))
}
