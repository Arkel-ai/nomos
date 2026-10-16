package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The verification gate, against a real Postgres: a run, its agent turn, its
// evidence spans, real legal_versions rows and stored revisions — the same
// rows the agent tools write. What is asserted is the policy the owner fixed:
// a pièce mismatch warns and keeps download open; a legal citation that is
// invented, out of evidence or of the wrong version is refused; a web URL
// without excerpt is a lead; an exhaustive request cannot claim completeness
// without full coverage; and another firm sees none of it.

// --- fixture ----------------------------------------------------------------

type agentFixture struct {
	t        *testing.T
	firm     string
	user     string
	repo     *u.TenantRepo
	dossier  types.Dossier
	conv     types.Conversation
	assist   types.Message
	run      types.Run
	turn     types.AgentTurn
	contract types.AgentTaskContract
	assetSHA map[string]string
	legal    []string // legal_documents ids to remove
	// piste is the official source the final gate revalidates against: the
	// fixture's articles are published there as stored.
	piste *fakePiste
}

func newAgentFixture(t *testing.T, label string) *agentFixture {
	t.Helper()
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres/Casbin not reachable — verification suite skipped")
	}
	f := &agentFixture{
		t:        t,
		firm:     "test_firm_verify_" + label + "_" + uuid.NewString()[:8],
		user:     "test_user_verify_" + label + "_" + uuid.NewString()[:8],
		assetSHA: map[string]string{},
		piste:    newFakePiste(t),
	}
	ctx := context.Background()
	if err := u.EnsureFirm(ctx, f.firm, "Cabinet "+label, f.user); err != nil {
		t.Fatalf("EnsureFirm: %v", err)
	}
	if err := u.AddRoleForUserInDomain(f.user, types.RoleUser, f.firm); err != nil {
		t.Fatalf("attach user: %v", err)
	}
	t.Cleanup(f.cleanup)
	f.repo = u.ForTenantID(ctx, f.firm)

	f.dossier = types.Dossier{ID: uuid.NewString(), Title: "Dossier de test", State: types.DossierStateActive, CreatedBy: f.user}
	f.must(f.repo.Create(&f.dossier))
	dossierID := f.dossier.ID
	f.conv = types.Conversation{ID: uuid.NewString(), DossierID: &dossierID, CreatedBy: f.user, State: types.ConversationStateActive}
	f.must(f.repo.Create(&f.conv))
	f.run = types.Run{
		ID: uuid.NewString(), ConversationID: &f.conv.ID, DossierID: dossierID, RequestedBy: f.user,
		WorkflowType: types.WorkflowNomosAgent, WorkflowVersion: types.WorkflowNomosAgentVersion,
		Instruction: "Établis la chronologie complète du dossier.", State: types.RunStateRunning,
	}
	f.must(f.repo.Create(&f.run))
	user := types.Message{ID: uuid.NewString(), ConversationID: f.conv.ID, Role: types.MessageRoleUser,
		Content: "Établis la chronologie. Le salaire convenu était de 3 100,00 € brut.", State: types.MessageStateComplete}
	f.must(f.repo.Create(&user))
	f.assist = types.Message{ID: uuid.NewString(), ConversationID: f.conv.ID, Role: types.MessageRoleAssistant,
		RunID: &f.run.ID, State: types.MessageStateStreaming}
	f.must(f.repo.Create(&f.assist))

	f.contract = types.AgentTaskContract{
		Version:     types.AgentTaskContractVersion,
		Instruction: f.run.Instruction,
		OutputKinds: []string{types.AgentOutputAnswer, types.AgentOutputChronology},
		Scope:       types.AgentSourceScope{Mode: types.ScopeAllPieces, DossierID: dossierID},
	}
	f.turn = types.AgentTurn{
		ID: uuid.NewString(), RunID: f.run.ID, ActorID: f.user, RequestID: uuid.NewString(),
		RequestSHA256: strings.Repeat("0", 64), ConversationID: &f.conv.ID, AssistantMessageID: &f.assist.ID,
		DossierID: &dossierID, DeadlineAt: time.Now().Add(time.Hour).Unix(),
		ControllerModel: "test/controller", PromptVersion: "test", ToolsetVersion: "test",
	}
	f.saveContract()
	f.must(f.repo.Create(&f.turn))
	return f
}

func (f *agentFixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("fixture: %v", err)
	}
}

// saveContract writes the contract onto the turn (before or after insert).
func (f *agentFixture) saveContract() {
	raw, err := json.Marshal(f.contract)
	f.must(err)
	f.turn.TaskContract = types.JSONB(raw)
	if f.turn.CreatedAt != 0 {
		f.must(f.repo.AgentTurns().Where("id = ?", f.turn.ID).Update("task_contract", f.turn.TaskContract).Error)
	}
}

func (f *agentFixture) cleanup() {
	db := u.DB
	for _, table := range []string{
		"agent_read_receipts", "agent_evidence_spans", "agent_document_revisions", "agent_verifications",
		"agent_web_sources", "agent_turns", "citations", "artifacts", "run_events", "run_questions",
		"llm_calls", "run_steps", "runs", "messages", "conversations", "pieces", "assets", "dossiers",
	} {
		db.Exec(fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ?", table), f.firm)
	}
	for _, id := range f.legal {
		db.Exec("DELETE FROM legal_versions WHERE legal_document_id = ?", id)
		db.Exec("DELETE FROM legal_documents WHERE id = ?", id)
	}
	db.Exec("DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ?", f.firm, f.firm)
	db.Exec("DELETE FROM casbin_rule WHERE v0 = ?", f.user)
	db.Exec("DELETE FROM firms WHERE id = ?", f.firm)
	_ = u.Enforcer.LoadPolicy()
}

func (f *agentFixture) addPiece(ordinal int, title string) types.Piece {
	sha := sha256Hex(uuid.NewString())
	asset := types.Asset{ID: uuid.NewString(), S3Key: u.DossierKey(f.firm, f.dossier.ID, uuid.NewString()+".pdf"),
		Kind: types.AssetKindOriginal, Mime: "application/pdf", SHA256: sha}
	f.must(f.repo.Create(&asset))
	p := types.Piece{ID: uuid.NewString(), DossierID: f.dossier.ID, SourceAssetID: asset.ID,
		OriginalName: fmt.Sprintf("piece-%d.pdf", ordinal), Title: title, Ordinal: ordinal,
		ClassificationState: types.PieceStateProposed, ExtractionVersion: "test-v1"}
	f.must(f.repo.Create(&p))
	f.assetSHA[p.ID] = sha
	return p
}

func (f *agentFixture) span(sourceType string, loc types.SourceLocator, quote, sourceSHA string, set func(*types.AgentEvidenceSpan)) types.AgentEvidenceSpan {
	raw, err := json.Marshal(loc)
	f.must(err)
	s := types.AgentEvidenceSpan{
		ID: uuid.NewString(), RunID: f.run.ID, SourceType: sourceType, Locator: types.JSONB(raw),
		Quote: quote, SourceSHA256: sourceSHA, RetrievalTool: "test", ToolCallID: "d1.c0",
	}
	if set != nil {
		set(&s)
	}
	s.SpanKey = types.EvidenceSpanKey(sourceType, s.ID, sourceSHA, loc, quote)
	f.must(f.repo.Create(&s))
	return s
}

func (f *agentFixture) pieceSpan(p types.Piece, page int, quote string) types.AgentEvidenceSpan {
	pid := p.ID
	return f.span(types.CitationSourcePiece, types.SourceLocator{Kind: types.LocatorPDF, Page: page, SegmentID: fmt.Sprintf("p%d", page)},
		quote, f.assetSHA[p.ID], func(s *types.AgentEvidenceSpan) { s.PieceID = &pid })
}

// addArticle stores a Légifrance-shaped article version valid [from, to),
// and publishes the same version on the fixture's official source.
func (f *agentFixture) addArticle(num, text string, from time.Time, to *time.Time) types.LegalVersion {
	doc := types.LegalDocument{ID: "test-doc-" + uuid.NewString(), Source: types.SourceLegifrance,
		SourceID: testLegiID(), Kind: "article", CitationLabel: "C. trav., art. " + num,
		CanonicalURL: "https://www.legifrance.gouv.fr/codes/article_lc/test"}
	f.must(u.DB.Create(&doc).Error)
	f.legal = append(f.legal, doc.ID)
	vf := from.Unix()
	ver := types.LegalVersion{ID: "test-ver-" + uuid.NewString(), LegalDocumentID: doc.ID, SourceVersion: doc.SourceID,
		Text: text, TextSHA256: sha256Hex(text), ValidFrom: &vf, State: types.LegalVersionInForce,
		SourceMeta: types.JSONB(fmt.Sprintf(`{"article_number":%q}`, strings.ReplaceAll(num, ". ", "")))}
	if to != nil {
		vt := to.Unix()
		ver.ValidTo = &vt
	}
	f.must(u.DB.Create(&ver).Error)
	f.piste.publishArticle(officialArticle(ver, doc.SourceID, num))
	return ver
}

func (f *agentFixture) legalSpan(ver types.LegalVersion, quote string) types.AgentEvidenceSpan {
	vid := ver.ID
	return f.span(types.CitationSourceLegifrance, types.SourceLocator{Kind: types.LocatorLegislation, VersionID: ver.ID},
		quote, ver.TextSHA256, func(s *types.AgentEvidenceSpan) { s.LegalVersionID = &vid })
}

func (f *agentFixture) webSpan(url, content, quote string) types.AgentEvidenceSpan {
	w := types.AgentWebSource{ID: uuid.NewString(), RunID: f.run.ID, URL: url, URLSHA256: sha256Hex(url),
		Host: hostOf(url), Title: "Fiche pratique", Content: content, ContentSHA256: sha256Hex(content),
		Extent: types.WebExtentExcerpt, ToolCallID: "d2.c0"}
	f.must(f.repo.Create(&w))
	wid := w.ID
	return f.span(types.CitationSourceWeb, types.SourceLocator{Kind: types.LocatorWeb, URL: url},
		quote, w.ContentSHA256, func(s *types.AgentEvidenceSpan) { s.WebSourceID = &wid })
}

func (f *agentFixture) addRevision(kind, title string, blocks ...types.DocumentBlock) types.AgentDocumentRevision {
	body := types.DocumentRevisionBody{SchemaVersion: types.DocumentSchemaVersion, Blocks: blocks}
	sum, err := types.DocumentContentSHA256(kind, title, body)
	f.must(err)
	raw, err := json.Marshal(body)
	f.must(err)
	rev := types.AgentDocumentRevision{ID: uuid.NewString(), RunID: f.run.ID, DocumentKey: kind + "-" + uuid.NewString()[:6],
		Revision: 1, Kind: kind, Title: title, ContentSHA256: sum, SchemaVersion: types.DocumentSchemaVersion,
		Body: types.JSONB(raw), CreatedByCallID: "d5.c" + uuid.NewString()[:4]}
	f.must(f.repo.Create(&rev))
	return rev
}

func (f *agentFixture) verify(revID string, final bool) types.VerificationReport {
	f.t.Helper()
	report, err := VerifierWith(f.piste.sources())(context.Background(), VerificationInput{
		TenantID: f.firm, RunID: f.run.ID, DocumentRevisionID: revID, Final: final,
	})
	if err != nil {
		f.t.Fatalf("VerifyOutput: %v", err)
	}
	return report
}

// store records a report the way the runtime does, inside a transaction.
func (f *agentFixture) store(report types.VerificationReport) types.AgentVerification {
	var row types.AgentVerification
	f.must(f.repo.Tx(func(tx *u.TenantRepo) error {
		var err error
		row, err = StoreVerification(tx, f.run.ID, report)
		return err
	}))
	return row
}

// reviewed records what a review cycle leaves behind: a verify_output report
// for this content, then an accepted controller dispatch after it.
func (f *agentFixture) reviewed(revID string) {
	f.store(f.verify(revID, false))
	n := 7
	call := types.LLMCall{ID: uuid.NewString(), RunID: &f.run.ID, ExecutionID: uuid.NewString(), Model: "test/controller",
		Purpose: agentPurpose, State: types.LLMCallStateSucceeded, AgentDispatch: &n, ActorID: f.user}
	f.must(f.repo.Create(&call))
}

// toolStep records a finished tool call the way the runtime does: a
// succeeded agent:tool step whose output is the call and its observation.
func (f *agentFixture) toolStep(callID, tool string, data map[string]any) {
	raw, err := json.Marshal(data)
	f.must(err)
	out, err := json.Marshal(agentToolRecord{
		Call:        types.AgentToolCall{ID: callID, Name: tool, Arguments: json.RawMessage(`{}`)},
		Observation: types.ToolObservation{CallID: callID, Tool: tool, Status: types.ObservationOK, Data: raw},
	})
	f.must(err)
	key := "agent:tool:" + strings.NewReplacer("d", "", ".c", ":").Replace(callID)
	f.must(f.repo.Create(&types.RunStep{
		ID: uuid.NewString(), RunID: f.run.ID, StepKey: key, Kind: stepAgentToolDB, State: types.StepStateSucceeded,
		IdempotencyKey: f.run.ID + ":" + key, Output: types.JSONB(out),
	}))
}

func row(id, text string, d types.PartialDate, claims ...types.DocumentClaim) types.DocumentBlock {
	return types.DocumentBlock{ID: id, Role: types.BlockChronologyRow, Text: text, Claims: claims,
		Chronology: &types.ChronologyRow{Date: types.ClaimDate{PartialDate: d, Role: types.DateRoleEvent}}}
}

func para(id, text string, claims ...types.DocumentClaim) types.DocumentBlock {
	return types.DocumentBlock{ID: id, Role: types.BlockParagraph, Text: text, Claims: claims}
}

func whole(text string) (int, int) { return 0, len([]rune(text)) }

func issueCodes(r types.VerificationReport) map[string]types.VerificationIssue {
	out := map[string]types.VerificationIssue{}
	for _, iss := range r.Issues {
		out[iss.Code] = iss
	}
	return out
}

func checkStatus(r types.VerificationReport, check string) string {
	for _, c := range r.Checks {
		if c.Check == check {
			return c.Status
		}
	}
	return ""
}

// --- pièces: warn, keep download -----------------------------------------------

func TestVerifyPieceMismatchWarnsAndKeepsDownload(t *testing.T) {
	f := newAgentFixture(t, "piece")
	lettre := f.addPiece(8, "Lettre de licenciement")
	span := f.pieceSpan(lettre, 1, "Par lettre recommandée du 5 mars 2025, nous vous notifions votre licenciement pour cause réelle et sérieuse.")

	good := "Notification du licenciement par lettre recommandée"
	s, e := whole(good)
	bad := "Envoi de la lettre de licenciement"
	bs, be := whole(bad)
	rev := f.addRevision(types.AgentOutputChronology, "Chronologie",
		row("r1", good, day(2025, 3, 5), types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: s, End: e,
			EvidenceIDs: []string{span.ID}, Dates: []types.ClaimDate{{PartialDate: day(2025, 3, 5), Role: types.DateRoleDocument}}}),
		row("r2", bad, day(2025, 3, 3), types.DocumentClaim{ID: "c2", Kind: types.ClaimFactual, Start: bs, End: be,
			EvidenceIDs: []string{span.ID}, Dates: []types.ClaimDate{{PartialDate: day(2025, 3, 3), Role: types.DateRoleEvent}}}),
	)

	report := f.verify(rev.ID, false)
	if report.Result != types.VerificationWarnings || !report.DownloadAllowed || report.Blocking != 0 || report.Warnings != 1 {
		t.Fatalf("pièce mismatch must warn with download allowed: result=%s download=%t warnings=%d blocking=%d issues=%+v",
			report.Result, report.DownloadAllowed, report.Warnings, report.Blocking, report.Issues)
	}
	iss := issueCodes(report)[types.IssuePieceDateNotFound]
	if iss.BlockID != "r2" || iss.ClaimID != "c2" || iss.Severity != types.SeverityWarning {
		t.Fatalf("warning not attached to its block and claim: %+v", iss)
	}
	want := "À vérifier — la date du 3 mars 2025 attribuée à la pièce n° 8 n'a pas été retrouvée dans le passage cité."
	if iss.Correction != want {
		t.Fatalf("inline warning = %q, want %q", iss.Correction, want)
	}
	if checkStatus(report, types.CheckPieceClaims) != types.CheckWarning {
		t.Fatalf("pièce check must read warning, never passed: %+v", report.Checks)
	}
	if report.ContentSHA256 != rev.ContentSHA256 || report.ScopeSHA256 == "" || report.EvidenceSHA256 == "" {
		t.Fatalf("report not bound to its hashes: %+v", report)
	}

	// Stored twice, the same report converges on one row bound to its hashes.
	a, b := f.store(report), f.store(report)
	if a.ID != b.ID || a.Result != types.VerificationWarnings || !a.DownloadAllowed {
		t.Fatalf("verification row: %+v / %+v", a, b)
	}

	// A pièce outside the frozen scope is not a content mismatch: it blocks.
	f.contract.Scope.Excluded = []types.ScopeEntry{{PieceID: lettre.ID, Provenance: types.ContractProvenance{Source: types.ContractFromUserMessage}}}
	f.saveContract()
	report = f.verify(rev.ID, false)
	if report.Result != types.VerificationBlocked || issueCodes(report)[types.IssueEvidenceUnauthorized].Code == "" {
		t.Fatalf("out-of-scope pièce must block: %+v", report.Issues)
	}
}

// --- printed figures: what the lawyer reads is what is checked ------------------

// A date or an amount printed in the text — in a block, in a chronology
// row's date column — must be stated by the source of the claim covering it,
// whether or not the model declared it in the claim's structured fields. A
// figure no claim covers, and a lawyer_provided claim the lawyer never
// wrote, are flagged « À vérifier »; download stays open.
func TestVerifyPrintedDatesAndAmountsNeedTheirSource(t *testing.T) {
	f := newAgentFixture(t, "printed")
	lettre := f.addPiece(8, "Lettre de licenciement")
	span := f.pieceSpan(lettre, 1, "Par lettre recommandée du 5 mars 2025, nous vous notifions votre licenciement. "+
		"L'indemnité de licenciement s'élève à 4 200,00 €.")

	notif := "Notification du licenciement"
	ns, ne := whole(notif)
	loose := "Le licenciement a été notifié le 14 mars 2025 ; l'indemnité versée s'élève à 9 999,00 €."
	faithful := "La lettre du 5 mars 2025 fixe l'indemnité à 4 200,00 €."
	fs, fe := whole(faithful)
	told := "Le client conteste la rupture."
	ts, te := whole(told)
	salary := "Le salaire convenu était de 3 100,00 € brut."
	ss, se := whole(salary)
	rev := f.addRevision(types.AgentOutputChronology, "Chronologie",
		// The row's date column says 3 March; the passage it cites says 5.
		row("r1", notif, day(2025, 3, 3), types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: ns, End: ne,
			EvidenceIDs: []string{span.ID}}),
		para("p1", loose),
		row("r2", "Entretien préalable", day(2025, 2, 20)),
		para("p2", faithful, types.DocumentClaim{ID: "c2", Kind: types.ClaimFactual, Start: fs, End: fe, EvidenceIDs: []string{span.ID}}),
		row("r3", notif, day(2025, 3, 5), types.DocumentClaim{ID: "c3", Kind: types.ClaimFactual, Start: ns, End: ne,
			EvidenceIDs: []string{span.ID}}),
		para("p3", told, types.DocumentClaim{ID: "c4", Kind: types.ClaimLawyerProvided, Start: ts, End: te}),
		para("p4", salary, types.DocumentClaim{ID: "c5", Kind: types.ClaimLawyerProvided, Start: ss, End: se}),
	)
	f.reviewed(rev.ID)
	report := f.verify(rev.ID, true)
	if report.Result != types.VerificationWarnings || !report.DownloadAllowed || report.Blocking != 0 {
		t.Fatalf("printed figures must warn, download allowed: result=%s warnings=%d blocking=%d issues=%+v",
			report.Result, report.Warnings, report.Blocking, report.Issues)
	}
	got := map[string][]string{}
	for _, iss := range report.Issues {
		got[iss.BlockID] = append(got[iss.BlockID], iss.Correction)
	}
	want := map[string][]string{
		"r1": {"À vérifier — la date du 3 mars 2025 attribuée à la pièce n° 8 n'a pas été retrouvée dans le passage cité."},
		"p1": {"À vérifier — la date du 14 mars 2025 n'est rattachée à aucun passage de pièce lu.",
			"À vérifier — le montant de 9 999,00 € n'est rattaché à aucun passage de pièce lu."},
		"r2": {"À vérifier — la date du 20 février 2025 n'est rattachée à aucun passage de pièce lu."},
		"p3": {warnLawyerProvided},
	}
	if len(got) != len(want) || report.Warnings != 5 {
		t.Fatalf("warnings by block = %v (total %d), want %v", got, report.Warnings, want)
	}
	for block, sentences := range want {
		if strings.Join(got[block], "|") != strings.Join(sentences, "|") {
			t.Fatalf("block %s: warnings %q, want %q", block, got[block], sentences)
		}
	}
	if checkStatus(report, types.CheckPieceClaims) != types.CheckWarning {
		t.Fatalf("the pièce check must read warning: %+v", report.Checks)
	}
}

// A derived amount rests on the run's own calculate call, and must be its
// result: an id naming another tool, or no call at all, or a figure the
// calculation did not produce is « À vérifier ».
func TestVerifyDerivedAmountsResolveTheirCalculation(t *testing.T) {
	f := newAgentFixture(t, "calc")
	contrat := f.addPiece(1, "Contrat de travail")
	span := f.pieceSpan(contrat, 2, "Le salaire mensuel brut est fixé à 3 100,00 €.")
	f.toolStep("d3.c0", types.ToolCalculate, map[string]any{
		"call_id": "d3.c0", "result": "4650.00", "unit": "EUR",
		"money":    types.Money{MinorUnits: 465000, Currency: "EUR"},
		"operands": []map[string]any{{"name": "salaire", "value": "3100.00", "unit": "EUR", "evidence_ids": []string{span.ID}}},
	})
	f.toolStep("d2.c0", types.ToolReadPiece, map[string]any{"piece_id": contrat.ID})

	derived := func(id, text, call string, minor int64, evidence ...string) types.DocumentBlock {
		s, e := whole(text)
		return para("b"+id, text, types.DocumentClaim{ID: "c" + id, Kind: types.ClaimDerived, Start: s, End: e,
			EvidenceIDs: evidence, CalculationCallID: call, Amounts: []types.Money{{MinorUnits: minor, Currency: "EUR"}}})
	}
	rev := f.addRevision(types.AgentOutputAnswer, "",
		derived("1", "Indemnité légale de licenciement : 4 650,00 €, calculée sur un salaire de 3 100,00 €.", "d3.c0", 465000, span.ID),
		derived("2", "Indemnité légale de licenciement : 12 345,67 €.", "d2.c0", 1234567, span.ID),
		derived("3", "Indemnité légale de licenciement : 5 000,00 €.", "d3.c0", 500000),
		derived("4", "Indemnité légale de licenciement : 7 777,00 €.", "d9.c9", 777700),
	)
	report := f.verify(rev.ID, true)
	if report.Blocking != 0 || !report.DownloadAllowed {
		t.Fatalf("a calculation mismatch warns, it does not block: %+v", report.Issues)
	}
	byBlock := map[string][]types.VerificationIssue{}
	for _, iss := range report.Issues {
		byBlock[iss.BlockID] = append(byBlock[iss.BlockID], iss)
	}
	if len(byBlock["b1"]) != 0 {
		t.Fatalf("an amount the calculation produced, from an operand it used: %+v", byBlock["b1"])
	}
	has := func(block, sentence string) types.VerificationIssue {
		t.Helper()
		for _, iss := range byBlock[block] {
			if iss.Correction == sentence {
				return iss
			}
		}
		t.Fatalf("block %s: no %q in %+v", block, sentence, byBlock[block])
		return types.VerificationIssue{}
	}
	const unknown = "À vérifier — le calcul cité pour ce montant n'existe pas dans ce travail."
	has("b2", unknown)
	has("b4", unknown)
	mismatch := has("b3", "À vérifier — le montant de 5 000,00 € ne correspond pas au résultat du calcul enregistré.")
	if mismatch.Code != types.IssuePieceCalculationMissing || len(mismatch.EvidenceIDs) != 1 || mismatch.EvidenceIDs[0] != span.ID {
		t.Fatalf("the mismatch must carry the calculation's operand evidence: %+v", mismatch)
	}
}

// --- legal: resolve or refuse --------------------------------------------------

const articleL1232 = "Tout licenciement pour motif personnel est motivé dans les conditions définies par le présent chapitre. " +
	"Il est justifié par une cause réelle et sérieuse."

func TestVerifyLegalCitationsResolveOrRefuse(t *testing.T) {
	f := newAgentFixture(t, "legal")
	inForce := f.addArticle("L. 1232-1", articleL1232, time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC), nil)
	old := f.addArticle("L. 1232-2", "Ancienne rédaction : l'employeur qui envisage de licencier un salarié le convoque avant toute décision.",
		time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC), ptrTime(time.Date(2016, 8, 10, 0, 0, 0, 0, time.UTC)))
	f.contract.StatuteApplicability = &types.DateInterval{From: &types.PartialDate{Precision: types.DatePrecisionMonth, Year: 2025, Month: 3}}
	f.saveContract()

	good := f.legalSpan(inForce, "Il est justifié par une cause réelle et sérieuse.")
	text := "Le licenciement doit reposer sur une cause réelle et sérieuse (article L. 1232-1 du code du travail)."
	s, e := whole(text)
	legal := func(ids ...string) types.DocumentClaim {
		return types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: s, End: e, EvidenceIDs: ids}
	}

	t.Run("resolved citation passes", func(t *testing.T) {
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", text, legal(good.ID)))
		report := f.verify(rev.ID, true)
		if report.Result != types.VerificationPassed || checkStatus(report, types.CheckLegalCitations) != types.CheckPassed {
			t.Fatalf("a resolved, applicable citation must pass: %+v", report.Issues)
		}
	})

	t.Run("invented reference in prose is refused", func(t *testing.T) {
		invented := text + " L'article L. 1232-19 impose en outre un préavis."
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", invented, legal(good.ID)))
		report := f.verify(rev.ID, false)
		iss := issueCodes(report)[types.IssueLegalReferenceUncited]
		if report.Result != types.VerificationBlocked || iss.BlockID != "b1" || !strings.Contains(iss.Correction, "L. 1232-19") {
			t.Fatalf("an uncited reference must block: %+v", report.Issues)
		}
	})

	t.Run("legal claim without legal evidence is refused", func(t *testing.T) {
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", "Le licenciement doit avoir une cause réelle et sérieuse.",
			types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: 0, End: 10}))
		if report := f.verify(rev.ID, false); issueCodes(report)[types.IssueLegalReferenceUncited].Code == "" || report.DownloadAllowed {
			t.Fatalf("model-memory law must block: %+v", report.Issues)
		}
	})

	t.Run("wrong version is refused", func(t *testing.T) {
		stale := f.legalSpan(old, "l'employeur qui envisage de licencier un salarié le convoque avant toute décision.")
		txt := "Le salarié est convoqué à un entretien préalable (article L. 1232-2 du code du travail)."
		ss, ee := whole(txt)
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", txt,
			types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: ss, End: ee, EvidenceIDs: []string{stale.ID}}))
		report := f.verify(rev.ID, false)
		if issueCodes(report)[types.IssueLegalVersionNotInForce].Code == "" || report.Result != types.VerificationBlocked {
			t.Fatalf("a version not in force in March 2025 must block: %+v", report.Issues)
		}
	})

	t.Run("evidence of another run is refused", func(t *testing.T) {
		other := newAgentFixture(t, "other")
		foreign := other.legalSpan(inForce, "Il est justifié par une cause réelle et sérieuse.")
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", text, legal(foreign.ID)))
		report := f.verify(rev.ID, false)
		if issueCodes(report)[types.IssueEvidenceUnknown].Code == "" || report.Result != types.VerificationBlocked {
			t.Fatalf("out-of-evidence citation must block: %+v", report.Issues)
		}
	})

	t.Run("quote absent from the version is refused", func(t *testing.T) {
		para2 := f.legalSpan(inForce, "Il est justifié par une faute grave du salarié dûment établie.")
		rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", text, legal(para2.ID)))
		if report := f.verify(rev.ID, false); issueCodes(report)[types.IssueLegalQuoteNotFound].Code == "" {
			t.Fatalf("a paraphrased quote must block: %+v", report.Issues)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

// Case law and texts cited the way lawyers write them — a chamber without
// « Cass. », an anaphoric « même code », a « loi du … » — are found in the
// prose and refused without a retrieved source, whatever kind the model gave
// the claim.
func TestVerifyRefusesUncitedReferencesInLawyersForms(t *testing.T) {
	f := newAgentFixture(t, "forms")
	for _, text := range []string{
		"Selon la chambre sociale (Soc., 25 septembre 2024), ce licenciement est sans cause réelle et sérieuse.",
		"L'article 1104 du même code impose la bonne foi.",
		"L'article 9 de la loi du 6 juillet 1989 s'applique.",
		"CA Paris, 12 mars 2023 : la clause est nulle.",
	} {
		s, e := whole(text)
		rev := f.addRevision(types.AgentOutputAnswer, "", para("a1", text,
			types.DocumentClaim{ID: "c1", Kind: types.ClaimAnalysis, Start: s, End: e}))
		report := f.verify(rev.ID, true)
		if report.Result != types.VerificationBlocked || issueCodes(report)[types.IssueLegalReferenceUncited].Code == "" {
			t.Fatalf("%q: an uncited reference must be refused, got %s %+v", text, report.Result, report.Issues)
		}
	}
}

// --- web: a URL without excerpt is a lead -------------------------------------

func TestVerifyWebWithoutExcerptIsALead(t *testing.T) {
	f := newAgentFixture(t, "web")
	text := "Le service public rappelle que le préavis court à compter de la présentation de la lettre."
	s, e := whole(text)
	claim := func(id string) types.DocumentClaim {
		return types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: s, End: e, EvidenceIDs: []string{id}}
	}

	lead := f.webSpan("https://www.service-public.fr/particuliers/vosdroits/F2835", "", "")
	rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", text, claim(lead.ID)))
	report := f.verify(rev.ID, false)
	iss := issueCodes(report)[types.IssueWebExcerptMissing]
	if iss.Severity != types.SeverityBlocking || report.DownloadAllowed || iss.EvidenceIDs[0] != lead.ID {
		t.Fatalf("a URL without excerpt must not support a claim: %+v", report.Issues)
	}

	excerpt := "Le préavis commence à courir à la date de première présentation de la lettre de licenciement."
	backed := f.webSpan("https://www.service-public.fr/particuliers/vosdroits/F2836", excerpt,
		"Le préavis commence à courir à la date de première présentation")
	rev = f.addRevision(types.AgentOutputAnswer, "", para("b1", text, claim(backed.ID)))
	if report := f.verify(rev.ID, false); report.Result != types.VerificationPassed {
		t.Fatalf("a quoted excerpt supports the claim: %+v", report.Issues)
	}
}

// --- coverage: no completeness claim without full coverage -------------------

func TestVerifyCoverageGatesExhaustiveClaims(t *testing.T) {
	f := newAgentFixture(t, "coverage")
	contrat := f.addPiece(1, "Contrat de travail")
	lettre := f.addPiece(2, "Lettre de licenciement")
	f.contract.Exhaustive = true
	f.saveContract()

	prev := PieceManifestFor
	t.Cleanup(func() { PieceManifestFor = prev })
	PieceManifestFor = func(_ context.Context, _ *u.TenantRepo, p types.Piece) (PieceManifest, error) {
		return PieceManifest{SourceSHA256: f.assetSHA[p.ID], ExtractionVersion: p.ExtractionVersion, Unit: "page",
			Segments: []ManifestSegment{{ID: "p1", Runes: 100, Readable: true}, {ID: "p2", Runes: 50, Readable: true}}}, nil
	}
	dispatch := 3
	receipt := func(p types.Piece, seg string, start, end int) {
		f.must(f.repo.Create(&types.AgentReadReceipt{ID: uuid.NewString(), RunID: f.run.ID, PieceID: p.ID,
			SourceSHA256: f.assetSHA[p.ID], ExtractionVersion: p.ExtractionVersion, SegmentID: seg,
			StartOffset: start, EndOffset: end, TextSHA256: sha256Hex(seg), ToolCallID: "d2.c" + uuid.NewString()[:4],
			RunStepID: "step", ConsumedDispatch: &dispatch}))
	}
	// The contract is read in full; the letter only up to half of page 2.
	receipt(contrat, "p1", 0, 60)
	receipt(contrat, "p1", 40, 100) // overlapping reads count once
	receipt(contrat, "p2", 0, 50)
	receipt(lettre, "p1", 0, 100)
	receipt(lettre, "p2", 0, 25)

	// Every row rests on a passage: the coverage gate is what is under test.
	signed := "Signature du contrat de travail"
	sig := f.pieceSpan(contrat, 1, "Le présent contrat est signé à Lyon en 2019.")
	signedRow := row("r1", signed, types.PartialDate{Precision: types.DatePrecisionYear, Year: 2019},
		types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: 0, End: len([]rune(signed)), EvidenceIDs: []string{sig.ID}})
	complete := f.addRevision(types.AgentOutputChronology, "Chronologie complète", signedRow)
	report := f.verify(complete.ID, false)
	iss := issueCodes(report)[types.IssueCoverageIncomplete]
	if report.Result != types.VerificationBlocked || iss.Severity != types.SeverityBlocking || !strings.Contains(iss.Correction, "n° 2") {
		t.Fatalf("an exhaustive chronology without full coverage must block: %+v", report.Issues)
	}
	if report.Coverage == nil || report.Coverage.Complete || report.Coverage.FullyRead != 1 || report.Coverage.TotalPieces != 2 ||
		report.Coverage.Segments.Missing != 1 || report.Coverage.Segments.Consumed != 3 {
		t.Fatalf("coverage = %+v", report.Coverage)
	}

	// Labelled incomplete, the same work is publishable as partial.
	partial := f.addRevision(types.AgentOutputChronology, "Chronologie (partielle)",
		types.DocumentBlock{ID: "l1", Role: types.BlockLimitation, Text: "La pièce n° 2 n'a pas été lue intégralement."},
		signedRow)
	report = f.verify(partial.ID, false)
	if report.Result != types.VerificationWarnings || !report.DownloadAllowed ||
		issueCodes(report)[types.IssueCoverageIncomplete].Severity != types.SeverityWarning {
		t.Fatalf("a result labelled incomplete is a warning: %+v", report.Issues)
	}

	// A read of an earlier extraction proves nothing about today's text.
	stale := lettre
	stale.ExtractionVersion = "test-v0"
	receipt(stale, "p2", 0, 50)
	report = f.verify(complete.ID, false)
	if report.Result != types.VerificationBlocked || report.Coverage.Pieces[1].State != types.CoveragePartial || !report.Coverage.Pieces[1].Mutated {
		t.Fatalf("a stale receipt must not complete coverage: %+v", report.Coverage.Pieces)
	}

	// Reading the rest completes coverage.
	receipt(lettre, "p2", 20, 50)
	report = f.verify(complete.ID, false)
	if report.Result != types.VerificationPassed || report.Coverage == nil || !report.Coverage.Complete {
		t.Fatalf("full coverage must pass: %+v / %+v", report.Issues, report.Coverage)
	}

	// Without the manifest nothing is ever complete.
	PieceManifestFor = nil
	if report := f.verify(complete.ID, false); report.Result != types.VerificationBlocked {
		t.Fatalf("unmeasurable coverage must not pass an exhaustive claim: %+v", report.Issues)
	}
}

// --- compétence --------------------------------------------------------------

func TestVerifyPinnedCompetenceSectionsAndClauses(t *testing.T) {
	f := newAgentFixture(t, "competence")
	def, err := json.Marshal(skillDefinition{
		Sections: []skillSection{
			{Title: "Chronologie", Required: true},
			{Title: "Dates incertaines", Required: true},
			{Title: "Observations", Required: false},
		},
		MandatoryClauses: []skillClause{{Label: "Traçabilité", Text: "Chaque date est suivie du numéro de la pièce qui l'établit."}},
	})
	f.must(err)
	skill := types.Skill{ID: uuid.NewString(), Slug: "chronologie-test", Name: "Chronologie", State: types.SkillStateActive, CurrentVersion: 1}
	f.must(f.repo.Create(&skill))
	sv := types.SkillVersion{ID: uuid.NewString(), SkillID: skill.ID, Version: 1, Definition: types.JSONB(def)}
	f.must(f.repo.Create(&sv))
	t.Cleanup(func() {
		u.DB.Exec("DELETE FROM skill_versions WHERE tenant_id = ?", f.firm)
		u.DB.Exec("DELETE FROM skills WHERE tenant_id = ?", f.firm)
	})
	f.contract.Competence = &types.PinnedCompetence{SkillID: skill.ID, SkillVersionID: sv.ID, Slug: skill.Slug, Version: 1}
	f.saveContract()

	heading := func(id, text string) types.DocumentBlock {
		return types.DocumentBlock{ID: id, Role: types.BlockHeading, Level: 1, Text: text}
	}
	// The row rests on a passage: the compétence is what is under test.
	hired := f.pieceSpan(f.addPiece(1, "Contrat de travail"), 1, "Embauche du salarié en 2019.")
	hiredRow := row("r1", "Embauche", types.PartialDate{Precision: types.DatePrecisionYear, Year: 2019},
		types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual, Start: 0, End: len([]rune("Embauche")), EvidenceIDs: []string{hired.ID}})
	missing := f.addRevision(types.AgentOutputChronology, "Chronologie",
		heading("h1", "1. Chronologie"),
		hiredRow)
	report := f.verify(missing.ID, false)
	codes := issueCodes(report)
	if report.Result != types.VerificationBlocked || !strings.Contains(codes[types.IssueCompetenceSection].Correction, "Dates incertaines") ||
		codes[types.IssueCompetenceClause].Code == "" {
		t.Fatalf("a missing required section and clause must block: %+v", report.Issues)
	}

	complete := f.addRevision(types.AgentOutputChronology, "Chronologie",
		heading("h1", "1. Chronologie"),
		hiredRow,
		heading("h2", "2. Dates incertaines"),
		para("p1", "Aucune. Chaque date est suivie du numéro de la pièce qui l’établit."))
	if report := f.verify(complete.ID, false); report.Result != types.VerificationPassed ||
		checkStatus(report, types.CheckCompetence) != types.CheckPassed {
		t.Fatalf("a document honouring the compétence passes: %+v", report.Issues)
	}
	// The answer in the thread is not the acte: the compétence does not apply.
	answer := f.addRevision(types.AgentOutputAnswer, "", para("a1", "La chronologie est prête."))
	if report := f.verify(answer.ID, false); checkStatus(report, types.CheckCompetence) != types.CheckNotApplicable {
		t.Fatalf("compétence on the answer: %+v", report.Checks)
	}
}

// Owner decision 5: an acte exists only under the lawyer's compétence.
func TestVerifyActeWithoutCompetenceBlocks(t *testing.T) {
	f := newAgentFixture(t, "acte")
	acte := f.addRevision(types.AgentOutputActe, "Requête",
		types.DocumentBlock{ID: "h1", Role: types.BlockHeading, Level: 1, Text: "Requête"},
		para("p1", "Projet de requête."))
	report := f.verify(acte.ID, false)
	if report.Result != types.VerificationBlocked || issueCodes(report)[types.IssueCompetenceSection].Code == "" {
		t.Fatalf("an acte without a compétence must block: %+v", report.Issues)
	}
}

// --- final gate --------------------------------------------------------------

func TestVerifyFinalGateNeedsReviewAndDecisions(t *testing.T) {
	f := newAgentFixture(t, "final")
	note := f.addRevision(types.AgentOutputLegalNote, "Note",
		types.DocumentBlock{ID: "h1", Role: types.BlockHeading, Level: 1, Text: "Analyse"},
		para("b1", "Analyse préliminaire du dossier.", types.DocumentClaim{ID: "c1", Kind: types.ClaimAnalysis, Start: 0, End: 7}))

	if report := f.verify(note.ID, true); issueCodes(report)[types.IssueReviewMissing].Code == "" {
		t.Fatalf("a document never verified before submission must block: %+v", report.Issues)
	}
	f.reviewed(note.ID)
	if report := f.verify(note.ID, true); report.Result != types.VerificationPassed || report.VerifierVersion != VerifierVersionFinal {
		t.Fatalf("after a review cycle the final gate passes: %+v", report.Issues)
	}

	f.contract.RequiredDecisions = []types.RequiredDecision{{Key: "periode", Description: "Période retenue", State: types.DecisionAsked}}
	f.saveContract()
	if report := f.verify(note.ID, true); issueCodes(report)[types.IssueLawyerDecisionOpen].Code == "" {
		t.Fatalf("an open lawyer decision must block delivery: %+v", report.Issues)
	}
	// verify_output (not final) does not hold the agent on delivery-only checks.
	if report := f.verify(note.ID, false); report.Result != types.VerificationPassed {
		t.Fatalf("non-final verification: %+v", report.Issues)
	}
	if _, err := VerifyOutput(context.Background(), VerificationInput{TenantID: f.firm, RunID: f.run.ID, DocumentRevisionID: "nope"}); !errors.Is(err, ErrVerificationTarget) {
		t.Fatalf("unknown revision: %v", err)
	}
}

// --- tenancy ----------------------------------------------------------------

func TestCitationAndCoverageRoutesAreTenantScoped(t *testing.T) {
	f := newAgentFixture(t, "owner")
	stranger := newAgentFixture(t, "stranger")
	p := f.addPiece(1, "Contrat")
	span := f.pieceSpan(p, 1, "Le présent contrat prend effet le 2 septembre 2019.")
	text := "Le contrat prend effet le 2 septembre 2019."
	s, e := whole(text)
	rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", text, types.DocumentClaim{ID: "c1", Kind: types.ClaimFactual,
		Start: s, End: e, EvidenceIDs: []string{span.ID}, Dates: []types.ClaimDate{{PartialDate: day(2019, 9, 2), Role: types.DateRoleEvent}}}))
	ver := f.store(f.verify(rev.ID, true))
	res, err := AgentPublisher.PublishResult(context.Background(), PublicationInput{
		TenantID: f.firm, RunID: f.run.ID, ActorID: f.user, AssistantMessageID: f.assist.ID,
		Answer: VerifiedRevision{Revision: rev, Verification: ver}, Outcome: types.AgentOutcomeCompleted,
		Fence: testFence(f),
	})
	if err != nil || len(res.CitationIDs) != 1 {
		t.Fatalf("PublishResult: %v %+v", err, res)
	}
	citationID := res.CitationIDs[0]

	// The owner sees them.
	status, body := ListRunCitations(ctxFor(f.firm, f.user), f.run.ID, CitationListQuery{MessageID: f.assist.ID})
	list, ok := body.(types.AgentCitationList)
	if status != http.StatusOK || !ok || list.Total != 1 || list.Items[0].SourceType != types.CitationSourcePiece ||
		list.Items[0].PieceOrdinal != 1 || list.Items[0].Number != 1 || list.Items[0].ClaimIDs[0] != "c1" {
		t.Fatalf("owner listing: %d %+v", status, body)
	}
	status, body = GetCitation(ctxFor(f.firm, f.user), citationID)
	detail, ok := body.(types.AgentCitationDetail)
	if status != http.StatusOK || !ok || !detail.SourceAvailable ||
		string([]rune(detail.Passage)[detail.HighlightStart:detail.HighlightEnd]) != span.Quote {
		t.Fatalf("owner detail: %d %+v", status, body)
	}
	if status, _ := RunCoverage(ctxFor(f.firm, f.user), f.run.ID); status != http.StatusOK {
		t.Fatalf("owner coverage: %d", status)
	}

	// Another firm gets the same 404 an unknown id gets.
	for name, call := range map[string]func() (int, any){
		"list": func() (int, any) {
			return ListRunCitations(ctxFor(stranger.firm, stranger.user), f.run.ID, CitationListQuery{})
		},
		"detail":   func() (int, any) { return GetCitation(ctxFor(stranger.firm, stranger.user), citationID) },
		"coverage": func() (int, any) { return RunCoverage(ctxFor(stranger.firm, stranger.user), f.run.ID) },
	} {
		if status, body := call(); status != http.StatusNotFound {
			t.Fatalf("cross-tenant %s: %d %+v", name, status, body)
		}
	}
	if status, _ := GetCitation(ctxFor(f.firm, f.user), uuid.NewString()); status != http.StatusNotFound {
		t.Fatalf("unknown citation: %d", status)
	}
}

// testFence stands in for the runner's lease-fenced transaction.
func testFence(f *agentFixture) func(context.Context, func(*u.TenantRepo) error) error {
	return func(_ context.Context, fn func(*u.TenantRepo) error) error { return f.repo.Tx(fn) }
}
