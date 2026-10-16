package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Official revalidation at publication, against
// a fake PISTE served by httptest and spoken to by the real connectors: a
// legal citation is published only when the official source returns the
// exact version cited, with the same text, applicable to the period; a
// changed text or another version is refused and never substituted; an
// unreachable source is retryable and publishes nothing; and a stored-only
// match is never labelled officially verified.

// fakePiste serves the two official calls the final gate makes — Légifrance
// getArticle and Judilibre decision — from what a test published, and can be
// switched down to stand for an outage.
type fakePiste struct {
	mu        sync.Mutex
	articles  map[string]connectors.Article // by the id a caller asks for
	decisions map[string]connectors.Decision
	down      bool
	calls     atomic.Int32
	srv       *httptest.Server
}

func newFakePiste(t *testing.T) *fakePiste {
	t.Helper()
	p := &fakePiste{articles: map[string]connectors.Article{}, decisions: map[string]connectors.Decision{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		p.calls.Add(1)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"maintenance"}`))
			return
		}
		switch r.URL.Path {
		case "/consult/getArticle":
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			a, ok := p.articles[body.ID]
			if !ok {
				_, _ = w.Write([]byte(`{"article":null}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"article": a})
		case "/decision":
			d, ok := p.decisions[r.URL.Query().Get("id")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(d)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// sources are the real connectors pointed at this server, one attempt each
// so an outage is observed at once.
func (p *fakePiste) sources() OfficialSources {
	opts := connectors.Options{ClientID: "id", ClientSecret: "secret",
		TokenURL: p.srv.URL + "/token", BaseURL: p.srv.URL, MaxAttempts: 1}
	return NewOfficialSources(connectors.NewLegifrance(opts), connectors.NewJudilibre(opts))
}

func (p *fakePiste) publishArticle(a connectors.Article) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.articles[a.ID] = a
}

// answerArticle makes the official source answer a request for id with a.
func (p *fakePiste) answerArticle(id string, a connectors.Article) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.articles[id] = a
}

func (p *fakePiste) publishDecision(d connectors.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decisions[d.ID] = d
}

func (p *fakePiste) withdrawDecision(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.decisions, id)
}

func (p *fakePiste) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// testLegiID is a well-formed, unique LEGIARTI identifier.
func testLegiID() string { return fmt.Sprintf("LEGIARTI%012d", rand.Int64N(1_000_000_000_000)) }

// officialArticle is the official publication of a stored article version.
func officialArticle(v types.LegalVersion, cid, num string) connectors.Article {
	a := connectors.Article{ID: v.SourceVersion, CID: cid, Num: strings.ReplaceAll(num, ". ", ""), Text: v.Text, Etat: "VIGUEUR",
		DateDebut: 32472144000000, DateFin: 32472144000000}
	if v.ValidFrom != nil {
		a.DateDebut = *v.ValidFrom * 1000
	}
	if v.ValidTo != nil {
		a.DateFin = *v.ValidTo * 1000
	}
	return a
}

// legalAnswer is an answer whose one claim rests on a legal span.
func (f *agentFixture) legalAnswer(span types.AgentEvidenceSpan) types.AgentDocumentRevision {
	text := "Le licenciement doit reposer sur une cause réelle et sérieuse (article L. 1232-1 du code du travail)."
	s, e := whole(text)
	return f.addRevision(types.AgentOutputAnswer, "", para("b1", text,
		types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: s, End: e, EvidenceIDs: []string{span.ID}}))
}

func (f *agentFixture) publish(rev types.AgentDocumentRevision, report types.VerificationReport) (PublicationResult, error) {
	return AgentPublisher.PublishResult(context.Background(), PublicationInput{
		TenantID: f.firm, RunID: f.run.ID, ActorID: f.user, AssistantMessageID: f.assist.ID,
		Answer:  VerifiedRevision{Revision: rev, Verification: f.store(report)},
		Outcome: types.AgentOutcomeCompleted, Fence: testFence(f),
	})
}

// inForce2025 is a fixture citing L. 1232-1, in force since 2008, for facts
// of March 2025.
func inForce2025(t *testing.T, label string) (*agentFixture, types.LegalVersion, types.AgentEvidenceSpan) {
	f := newAgentFixture(t, label)
	ver := f.addArticle("L. 1232-1", articleL1232, time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC), nil)
	f.contract.StatuteApplicability = &types.DateInterval{From: &types.PartialDate{Precision: types.DatePrecisionMonth, Year: 2025, Month: 3}}
	f.saveContract()
	return f, ver, f.legalSpan(ver, "Il est justifié par une cause réelle et sérieuse.")
}

func storedVersion(t *testing.T, id string) types.LegalVersion {
	t.Helper()
	var v types.LegalVersion
	if err := u.DB.Where("id = ?", id).Take(&v).Error; err != nil {
		t.Fatalf("stored version: %v", err)
	}
	return v
}

func TestOfficialRevalidationPublishesOnlyAMatchedVersion(t *testing.T) {
	f, ver, span := inForce2025(t, "officialok")
	// Légifrance serves the plain text with its own line breaks: spacing
	// may differ, nothing else.
	official := officialArticle(ver, ver.SourceVersion, "L1232-1")
	official.Text = strings.Replace(articleL1232, ". ", ".\n", 1)
	f.piste.publishArticle(official)

	rev := f.legalAnswer(span)
	report := f.verify(rev.ID, true)
	if report.Result != types.VerificationPassed || len(report.Official) != 1 ||
		report.Official[0].EvidenceID != span.ID || report.Official[0].OfficialID != ver.SourceVersion {
		t.Fatalf("a version the official source confirms must pass with its round-trip recorded: %+v %+v", report.Issues, report.Official)
	}
	if _, err := f.publish(rev, report); err != nil {
		t.Fatalf("PublishResult: %v", err)
	}
	var cit types.Citation
	f.must(f.repo.Citations().Where("run_id = ? AND evidence_span_id = ?", f.run.ID, span.ID).Take(&cit).Error)
	var checks []types.CitationCheck
	f.must(json.Unmarshal(cit.Checks, &checks))
	for _, c := range checks {
		if c.Check == types.CitationCheckOfficialMatched && (c.Status != types.CitationCheckPassed || c.CheckedAt == nil) {
			t.Fatalf("official check on the published citation: %+v", c)
		}
	}
	if cit.VerificationState == types.CitationLawyerVerified {
		t.Fatal("no automated operation writes lawyer_verified")
	}
	// The non-final verify_output never calls the official source.
	calls := f.piste.calls.Load()
	if r := f.verify(rev.ID, false); len(r.Official) != 0 || f.piste.calls.Load() != calls {
		t.Fatalf("verify_output made an official round-trip: %+v", r.Official)
	}
}

func TestOfficialRevalidationRefusesAChangedText(t *testing.T) {
	f, ver, span := inForce2025(t, "officialchanged")
	changed := officialArticle(ver, ver.SourceVersion, "L1232-1")
	changed.Text = articleL1232 + " Il est notifié par lettre recommandée avec avis de réception."
	f.piste.publishArticle(changed)

	rev := f.legalAnswer(span)
	report := f.verify(rev.ID, true)
	iss := issueCodes(report)[types.IssueLegalOfficialMismatch]
	if report.Result != types.VerificationBlocked || iss.Code == "" || iss.Retryable || len(report.Official) != 0 ||
		iss.BlockID != "b1" || len(iss.EvidenceIDs) != 1 || iss.EvidenceIDs[0] != span.ID {
		t.Fatalf("a changed official text must block, not retryable: %+v", report.Issues)
	}
	if onlyRetryableBlocking([]types.VerificationReport{report}) {
		t.Fatal("a changed official text is not something waiting fixes")
	}
	if _, err := f.publish(rev, report); !errors.Is(err, ErrPublicationRefused) {
		t.Fatalf("a refused answer must not publish: %v", err)
	}
	if n := testCount(t, "citations", "tenant_id = ?", f.firm); n != 0 {
		t.Fatalf("%d citations bound for a refused answer", n)
	}
	if got := storedVersion(t, ver.ID); got.Text != articleL1232 || got.TextSHA256 != ver.TextSHA256 {
		t.Fatal("the official answer rewrote the stored version")
	}
}

// C. trav., art. L. 3123-31 as Légifrance publishes it: the HTML body (the
// code tree serves the same one, and the corpus loader stores it through
// htmlText) and getArticle's plain texte, which pads the cross-reference
// link with a space before the comma. Licence Ouverte 2.0.
const (
	l3123_31HTML = `<p>A défaut d'accord prévu à l'article <a href="/affichCodeArticle.do?cidTexte=LEGITEXT000006072050&idArticle=LEGIARTI000006902564&dateTexte=&categorieLien=cid" title="Code du travail - art. L3123-24 (V)">L. 3123-24</a>, ` +
		`toute modification de la répartition de la durée du travail entre les jours de la semaine ou les semaines du mois est notifiée au salarié ` +
		`au moins sept jours ouvrés avant la date à laquelle elle doit avoir lieu.</p><p></p><p></p>`
	l3123_31Plain = "A défaut d'accord prévu à l'article L. 3123-24 , toute modification de la répartition de la durée du travail entre les jours " +
		"de la semaine ou les semaines du mois est notifiée au salarié au moins sept jours ouvrés avant la date à laquelle elle doit avoir lieu."
	// l3123_31Loader is what the corpus loader stored for LEGIARTI000033019939.
	l3123_31Loader = "A défaut d'accord prévu à l'article L. 3123-24, toute modification de la répartition de la durée du travail entre les jours " +
		"de la semaine ou les semaines du mois est notifiée au salarié au moins sept jours ouvrés avant la date à laquelle elle doit avoir lieu."
)

// A version the corpus loader stored is compared with the official HTML
// rendering it was made from, a version read_law stored by LEGIARTI id with
// the plain one: both revalidate, and a text changed anywhere else is still
// refused.
func TestOfficialRevalidationComparesLikeWithLike(t *testing.T) {
	if got := (connectors.CodeArticle{Content: l3123_31HTML}).Text(); got != l3123_31Loader {
		t.Fatalf("the loader no longer renders the code tree as stored: %q", got)
	}
	p := newFakePiste(t)
	id := testLegiID()
	from := int64(1470787200)
	p.publishArticle(connectors.Article{ID: id, CID: id, Num: "L3123-31", Text: l3123_31Plain, TextHTML: l3123_31HTML,
		Etat: "VIGUEUR", DateDebut: from * 1000, DateFin: 32472144000000})
	at := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC).Unix()
	basis := LegalTemporalBasis{ApplicabilityFrom: &at, ApplicabilityTo: &at}
	check := func(text, want string) {
		t.Helper()
		src := AgentLegalSource{
			Document: types.LegalDocument{Source: types.SourceLegifrance, SourceID: id, Kind: legalKindArticle},
			Version:  types.LegalVersion{ID: "v", SourceVersion: id, Text: text, ValidFrom: &from},
		}
		if out := RevalidateOfficial(context.Background(), p.sources(), src, basis); out.Status != want {
			t.Fatalf("revalidating %q: %+v, want %s", text, out, want)
		}
	}
	check(l3123_31Loader, OfficialMatched)
	check(l3123_31Plain, OfficialMatched)
	check(strings.Replace(l3123_31Loader, "sept jours", "huit jours", 1), OfficialMismatch)
	check(strings.Replace(l3123_31Plain, "sept jours", "huit jours", 1), OfficialMismatch)
	check(strings.Replace(l3123_31Loader, "L. 3123-24,", "L. 3123-24.", 1), OfficialMismatch)
}

func TestOfficialRevalidationConnectorDownIsRetryable(t *testing.T) {
	f, _, span := inForce2025(t, "officialdown")
	rev := f.legalAnswer(span)

	f.piste.setDown(true)
	report := f.verify(rev.ID, true)
	iss := issueCodes(report)[types.IssueLegalOfficialDown]
	if report.Result != types.VerificationBlocked || iss.Code == "" || !iss.Retryable || iss.Severity != types.SeverityBlocking {
		t.Fatalf("an unreachable official source must be a blocking, retryable issue: %+v", report.Issues)
	}
	if !onlyRetryableBlocking([]types.VerificationReport{report}) {
		t.Fatal("the gate must treat an outage alone as retryable")
	}
	if _, err := f.publish(rev, report); !errors.Is(err, ErrPublicationRefused) {
		t.Fatalf("nothing may publish while the source is down: %v", err)
	}
	if n := testCount(t, "citations", "tenant_id = ?", f.firm); n != 0 {
		t.Fatalf("%d citations bound while the source was down", n)
	}

	// The same revision passes once the source answers again.
	f.piste.setDown(false)
	if again := f.verify(rev.ID, true); again.Result != types.VerificationPassed || len(again.Official) != 1 {
		t.Fatalf("after the outage: %+v", again.Issues)
	}

	// No source configured at all: blocking, and not retryable.
	none, err := VerifierWith(nil)(context.Background(), VerificationInput{
		TenantID: f.firm, RunID: f.run.ID, DocumentRevisionID: rev.ID, Final: true})
	if err != nil {
		t.Fatal(err)
	}
	if iss := issueCodes(none)[types.IssueLegalOfficialDown]; iss.Code == "" || iss.Retryable || none.DownloadAllowed {
		t.Fatalf("an unconfigured source: %+v", none.Issues)
	}
}

func TestOfficialRevalidationNeverSubstitutesTheHistoricalVersion(t *testing.T) {
	f := newAgentFixture(t, "officialhist")
	cut := time.Date(2016, 8, 10, 0, 0, 0, 0, time.UTC)
	old := f.addArticle("L. 1232-2", "Ancienne rédaction : l'employeur qui envisage de licencier un salarié le convoque avant toute décision.",
		time.Date(2008, 5, 1, 0, 0, 0, 0, time.UTC), &cut)
	f.contract.StatuteApplicability = &types.DateInterval{From: &types.PartialDate{Precision: types.DatePrecisionYear, Year: 2012}}
	f.saveContract()
	span := f.legalSpan(old, "l'employeur qui envisage de licencier un salarié le convoque avant toute décision.")
	txt := "En 2012, le salarié devait être convoqué avant toute décision (article L. 1232-2 du code du travail)."
	s, e := whole(txt)
	rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", txt,
		types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: s, End: e, EvidenceIDs: []string{span.ID}}))

	// As stored, the 2012 version is applicable and the official source
	// agrees.
	if r := f.verify(rev.ID, true); r.Result != types.VerificationPassed {
		t.Fatalf("the historical version, confirmed officially: %+v", r.Issues)
	}

	// Asked for the 2012 version, the source answers with today's.
	current := connectors.Article{ID: testLegiID(), CID: old.SourceVersion, Num: "L1232-2", Etat: "VIGUEUR",
		Text:      "Rédaction actuelle : l'employeur convoque le salarié à un entretien préalable par lettre recommandée.",
		DateDebut: cut.UnixMilli(), DateFin: 32472144000000}
	f.piste.answerArticle(old.SourceVersion, current)
	r := f.verify(rev.ID, true)
	iss := issueCodes(r)[types.IssueLegalOfficialMismatch]
	if r.Result != types.VerificationBlocked || iss.Code == "" || !strings.Contains(iss.Correction, "jamais") || len(r.Official) != 0 {
		t.Fatalf("another version in place of the one cited must be refused: %+v", r.Issues)
	}
	if got := storedVersion(t, old.ID); got.Text != old.Text || got.SourceVersion != old.SourceVersion {
		t.Fatal("the stored historical version was replaced")
	}

	// The same version, but officially ended before the period requested.
	ended := officialArticle(old, old.SourceVersion, "L1232-2")
	ended.DateFin = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	f.piste.publishArticle(ended)
	if r := f.verify(rev.ID, true); issueCodes(r)[types.IssueLegalOfficialMismatch].Code == "" {
		t.Fatalf("a version the official source says was not in force in 2012 must be refused: %+v", r.Issues)
	}
}

func TestOfficialRevalidationOfADecision(t *testing.T) {
	f := newAgentFixture(t, "officialdec")
	decisionID := "test" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	text := "Attendu que le licenciement d'un salarié doit reposer sur une cause réelle et sérieuse ; " +
		"que la cour d'appel, qui a constaté l'absence de grief établi, a légalement justifié sa décision."
	decided := time.Date(2022, 1, 12, 0, 0, 0, 0, time.UTC).Unix()
	doc := types.LegalDocument{ID: "test-doc-" + uuid.NewString(), Source: types.SourceJudilibre, SourceID: decisionID,
		Kind: legalKindDecision, CitationLabel: "Cass. soc., décision de test"}
	f.must(u.DB.Create(&doc).Error)
	f.legal = append(f.legal, doc.ID)
	ver := types.LegalVersion{ID: "test-ver-" + uuid.NewString(), LegalDocumentID: doc.ID, SourceVersion: "2022-02-01",
		Text: text, TextSHA256: sha256Hex(text), DecisionDate: &decided, State: types.LegalVersionInForce}
	f.must(u.DB.Create(&ver).Error)
	vid := ver.ID
	span := f.span(types.CitationSourceJudilibre, types.SourceLocator{Kind: types.LocatorDecision, DecisionID: decisionID},
		"le licenciement d'un salarié doit reposer sur une cause réelle et sérieuse", ver.TextSHA256,
		func(s *types.AgentEvidenceSpan) { s.LegalVersionID = &vid })
	claim := "La jurisprudence exige une cause réelle et sérieuse."
	s, e := whole(claim)
	rev := f.addRevision(types.AgentOutputAnswer, "", para("b1", claim,
		types.DocumentClaim{ID: "c1", Kind: types.ClaimLegal, Start: s, End: e, EvidenceIDs: []string{span.ID}}))

	f.piste.publishDecision(connectors.Decision{ID: decisionID, DecisionDate: "2022-01-12", UpdateDate: "2022-02-01",
		Text: strings.ReplaceAll(text, " ; ", " ;\n")})
	if r := f.verify(rev.ID, true); r.Result != types.VerificationPassed || len(r.Official) != 1 || r.Official[0].OfficialID != decisionID {
		t.Fatalf("a decision the official source confirms: %+v %+v", r.Issues, r.Official)
	}
	f.piste.publishDecision(connectors.Decision{ID: decisionID, DecisionDate: "2022-01-12", Text: text + " Rejette le pourvoi."})
	if r := f.verify(rev.ID, true); issueCodes(r)[types.IssueLegalOfficialMismatch].Code == "" {
		t.Fatalf("a changed official decision text must be refused: %+v", r.Issues)
	}
	f.piste.withdrawDecision(decisionID)
	if r := f.verify(rev.ID, true); issueCodes(r)[types.IssueLegalOfficialMismatch].Code == "" {
		t.Fatalf("a decision the official source does not know must be refused: %+v", r.Issues)
	}
}

// A report without the official round-trip — an older verifier's, or a
// stored-only check — is no authority to publish a legal citation.
func TestPublicationRefusesALegalCitationWithoutOfficialRevalidation(t *testing.T) {
	f, _, span := inForce2025(t, "officialmissing")
	rev := f.legalAnswer(span)
	report := f.verify(rev.ID, true)
	if report.Result != types.VerificationPassed {
		t.Fatalf("fixture: %+v", report.Issues)
	}
	report.Official = nil
	if _, err := f.publish(rev, report); !errors.Is(err, errOfficialNotRevalidated) {
		t.Fatalf("a stored-only match was published: %v", err)
	}
	if n := testCount(t, "citations", "tenant_id = ?", f.firm); n != 0 {
		t.Fatalf("%d citations bound without official revalidation", n)
	}
}

// The final gate's retryable path: an outage alone goes back to the agent as
// source_unavailable, the next decision waits, and a later submission of the
// same revision publishes.
func TestAgentGateWaitsWhenTheOfficialSourceIsDown(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Le préavis est d'un mois."})})
	}, submitTool())
	prevDelay := agentSourceRetryDelay
	agentSourceRetryDelay = 2 * time.Second
	t.Cleanup(func() { agentSourceRetryDelay = prevDelay })
	passing := passingVerifier(0)
	var calls atomic.Int32
	VerifyOutput = func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
		report, err := passing(ctx, in)
		if err == nil && calls.Add(1) == 1 {
			report.Result, report.DownloadAllowed, report.Blocking = types.VerificationBlocked, false, 1
			report.Issues = []types.VerificationIssue{{
				Code: types.IssueLegalOfficialDown, Severity: types.SeverityBlocking, Check: types.CheckLegalCitations,
				BlockID: "b1", Retryable: true, Correction: officialDown,
			}}
		}
		return report, err
	}

	_, res := h.post(t, "Quel est le délai de préavis applicable ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != 2 {
		t.Fatalf("run = %s/%s after %d dispatches", run.State, run.FailureCode, h.gw.hits())
	}
	if !strings.Contains(h.gw.requestText(2), types.ToolCodeSourceUnavailable) {
		t.Fatal("dispatch 2 does not carry the retryable source_unavailable observation")
	}
	steps, err := agentRunSteps(h.repo(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate, next := steps[agentGateKey(1)], steps[agentDecisionKey(2)]
	if gate.FinishedAt == nil || next.AvailableAt.Unix()-*gate.FinishedAt < 1 {
		t.Fatalf("the next decision did not wait (gate finished %v, decision available %s)", gate.FinishedAt, next.AvailableAt)
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want only after the source answered", h.publisher.calls.Load())
	}
}

// officialDownVerifier blocks the first down submissions on an unreachable
// official source (retryable), then passes.
func officialDownVerifier(down int) func(context.Context, VerificationInput) (types.VerificationReport, error) {
	passing := passingVerifier(0)
	var calls atomic.Int32
	return func(ctx context.Context, in VerificationInput) (types.VerificationReport, error) {
		report, err := passing(ctx, in)
		if err == nil && int(calls.Add(1)) <= down {
			report.Result, report.DownloadAllowed, report.Blocking = types.VerificationBlocked, false, 1
			report.Issues = []types.VerificationIssue{{
				Code: types.IssueLegalOfficialDown, Severity: types.SeverityBlocking, Check: types.CheckLegalCitations,
				BlockID: "b1", Retryable: true, Correction: officialDown,
			}}
		}
		return report, err
	}
}

// An outage is not a blockage: the agent resubmitting the same revision, as
// the gate tells it to, is not counted toward no_progress, and the answer
// publishes once the source is back.
func TestAgentGateOutageIsNotNoProgress(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Le préavis est d'un mois."})})
	}, submitTool())
	prevDelay := agentSourceRetryDelay
	agentSourceRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { agentSourceRetryDelay = prevDelay })
	down := types.MaxAgentRepeatedFailures
	VerifyOutput = officialDownVerifier(down)

	_, res := h.post(t, "Quel est le délai de préavis applicable ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateSucceeded || h.gw.hits() != down+1 {
		t.Fatalf("run = %s/%s after %d dispatches, want succeeded after %d", run.State, run.FailureCode, h.gw.hits(), down+1)
	}
	if h.publisher.calls.Load() != 1 {
		t.Fatalf("publisher called %d times, want once the source answered", h.publisher.calls.Load())
	}
}

// An outage that outlasts its own bound ends the turn with a code and an
// explanation that name the official source — never no_progress, whose text
// blames the request and calls a resend futile.
func TestAgentGateOutageEndsWithItsOwnExplanation(t *testing.T) {
	h := newAgentHarness(t, func(n int, _ map[string]any) (int, string) {
		return 200, toolCallsResponse("", fakeCall{ID: fmt.Sprintf("call_s%d", n), Name: types.ToolSubmitResult,
			Args: withNote("Je soumets la réponse.", map[string]any{"answer": "Le préavis est d'un mois."})})
	}, submitTool())
	prevDelay := agentSourceRetryDelay
	agentSourceRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { agentSourceRetryDelay = prevDelay })
	VerifyOutput = officialDownVerifier(1000)

	_, res := h.post(t, "Quel est le délai de préavis applicable ?")
	run := h.waitTerminal(t, res.Agent.RunID)
	if run.State != types.RunStateFailed || run.FailureCode != agentCodeOfficialSourceUnavailable {
		t.Fatalf("run = %s/%s, want failed/%s", run.State, run.FailureCode, agentCodeOfficialSourceUnavailable)
	}
	if h.gw.hits() != types.MaxAgentSourceOutages {
		t.Fatalf("gateway hits = %d, want the outage bound of %d", h.gw.hits(), types.MaxAgentSourceOutages)
	}
	if turn := h.turn(t, run.ID); turn.Outcome != types.AgentOutcomeFailed {
		t.Fatalf("turn outcome = %q, want %s", turn.Outcome, types.AgentOutcomeFailed)
	}
	msg := h.message(t, res.AssistantMessage.ID)
	if !strings.Contains(msg.Content, "source officielle") || !strings.Contains(msg.Content, "relancée") ||
		strings.Contains(msg.Content, "à l'identique") || h.publisher.calls.Load() != 0 {
		t.Fatalf("explanation = %q (publisher calls %d)", msg.Content, h.publisher.calls.Load())
	}
}

// Live smoke on the PISTE sandbox, only with credentials: an article fetched
// by its exact id revalidates against itself, whitespace aside, and an
// unknown id is refused rather than reported unavailable.
func TestOfficialRevalidationLiveSandbox(t *testing.T) {
	testmode.Live(t, "official revalidation smoke (live PISTE)")
	setupConversationSuite()
	if !legifranceReady() {
		testmode.Missing(t, "no PISTE credentials — live official revalidation smoke skipped")
	}
	ctx := context.Background()
	const id = "LEGIARTI000036212577" // C. trav., art. R1232-13
	a, err := PisteSources().Article(ctx, id)
	if err != nil {
		t.Fatalf("getArticle: %v", err)
	}
	from, to := a.Validity()
	src := AgentLegalSource{
		Document: types.LegalDocument{Source: types.SourceLegifrance, SourceID: a.CID, Kind: legalKindArticle},
		Version:  types.LegalVersion{ID: "live", SourceVersion: id, Text: strings.Join(strings.Fields(a.Text), " "), ValidFrom: from, ValidTo: to},
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	s, e := day.Unix(), day.AddDate(0, 0, 1).Unix()
	basis := LegalTemporalBasis{ApplicabilityFrom: &s, ApplicabilityTo: &e}
	if out := RevalidateOfficial(ctx, PisteSources(), src, basis); out.Status != OfficialMatched || out.OfficialID != id {
		t.Fatalf("live revalidation: %+v", out)
	}
	src.Version.SourceVersion = "LEGIARTI000099999999"
	if out := RevalidateOfficial(ctx, PisteSources(), src, basis); out.Status != OfficialMismatch {
		t.Fatalf("an unknown version must be refused: %+v", out)
	}

	// A version the corpus loader stored from the code tree, whose plain
	// getArticle texte pads a cross-reference link before its comma.
	const loaderID = "LEGIARTI000033019939" // C. trav., art. L3123-31
	official, err := PisteSources().Article(ctx, loaderID)
	if err != nil {
		t.Fatalf("getArticle: %v", err)
	}
	if officialTextSHA256(official.Text) == officialTextSHA256(l3123_31Loader) {
		t.Log("getArticle's plain texte no longer pads the link: the loader case is covered by the HTML comparison alone")
	}
	from, to = official.Validity()
	loaded := AgentLegalSource{
		Document: types.LegalDocument{Source: types.SourceLegifrance, SourceID: official.CID, Kind: legalKindArticle},
		Version:  types.LegalVersion{ID: "live-loader", SourceVersion: loaderID, Text: l3123_31Loader, ValidFrom: from, ValidTo: to},
	}
	if out := RevalidateOfficial(ctx, PisteSources(), loaded, basis); out.Status != OfficialMatched || out.OfficialID != loaderID {
		t.Fatalf("a loader-stored version must revalidate against getArticle: %+v", out)
	}
}
