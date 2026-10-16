package agent

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Arkel-ai/nomos/api/types"
)

// Outcome is everything collected about one finished trial (runner.go). The
// oracle is evaluated over this and nothing else. The report carries its
// shape; the complete record — revision bodies, texts, evidence map — is
// written beside the trial's outputs (outcome.json, see Record), so a trial
// can be diagnosed or re-judged without running anything.
type Outcome struct {
	RunID       string `json:"run_id"`
	RunState    string `json:"run_state"`
	FailureCode string `json:"failure_code,omitempty"`
	// TurnOutcome is agent_turns.outcome; Outcome is the trial's: the turn's,
	// or `asked` when it parked on a question the runner did not answer.
	TurnOutcome string `json:"turn_outcome"`
	Outcome     string `json:"outcome"`

	Dispatches      int    `json:"dispatches"`       // agent_turns.dispatches
	ControllerCalls int    `json:"controller_calls"` // llm_calls rows of purpose agent
	Exhaustive      bool   `json:"exhaustive"`
	ReviewCalls     int    `json:"review_calls"`
	WebSearches     int    `json:"web_searches"`
	PromptVersion   string `json:"prompt_version"`
	ToolsetVersion  string `json:"toolset_version"`

	Answer       DeliveredAnswer    `json:"answer"`
	Verification *VerificationFacts `json:"verification,omitempty"`
	Coverage     *CoverageFacts     `json:"coverage,omitempty"`
	// LiveScopeSHA256 is the scope hash recomputed once the trial is over.
	LiveScopeSHA256 string `json:"live_scope_sha256,omitempty"`

	Revisions []RevisionFacts `json:"revisions"`
	Artifacts []ArtifactFacts `json:"artifacts"`
	Citations []CitationFacts `json:"citations"`
	// EvidencePieces maps an evidence span to the fixture file it quotes.
	EvidencePieces map[string]string `json:"-"`

	Tools         map[string]*ToolFacts `json:"tools"`
	PublicQueries []string              `json:"public_queries,omitempty"`
	Questions     []QuestionFacts       `json:"questions,omitempty"`
	AnswerRounds  int                   `json:"answer_rounds"`
	Rejections    int                   `json:"decision_rejections"`
	ErrorRows     int                   `json:"error_observations"`

	// PublicQueries above are the search_* queries that left, or may have
	// left, the cabinet (activity rows); RejectedQueries counts those the
	// confidentiality gate kept in. ForwardedQueries are the web-search
	// request texts the proxy actually forwarded to the gateway.
	RejectedQueries  int      `json:"rejected_queries,omitempty"`
	ForwardedQueries []string `json:"forwarded_queries,omitempty"`

	LatencyMS int64 `json:"latency_ms"`
}

// OutcomeRecord is the complete outcome, the fields the report leaves out
// included.
type OutcomeRecord struct {
	Outcome
	AnswerText      string                                `json:"answer_text"`
	RevisionBodies  map[string]types.DocumentRevisionBody `json:"revision_bodies"`
	EvidencePieces  map[string]string                     `json:"evidence_pieces"`
	ArtifactTexts   map[string]string                     `json:"artifact_texts"`
	QuestionPrompts map[string]string                     `json:"question_prompts"`
}

// Record is the complete outcome.
func (o Outcome) Record() OutcomeRecord {
	rec := OutcomeRecord{Outcome: o, AnswerText: o.Answer.Text, EvidencePieces: o.EvidencePieces,
		RevisionBodies: map[string]types.DocumentRevisionBody{}, ArtifactTexts: map[string]string{}, QuestionPrompts: map[string]string{}}
	for _, r := range o.Revisions {
		rec.RevisionBodies[r.ID] = r.Body
	}
	for _, a := range o.Artifacts {
		rec.ArtifactTexts[a.ID] = a.Text
	}
	for _, q := range o.Questions {
		rec.QuestionPrompts[q.Key] = q.Prompt
	}
	return rec
}

// DeliveredAnswer is the assistant message the lawyer received.
type DeliveredAnswer struct {
	Text            string `json:"-"`
	Chars           int    `json:"chars"`
	State           string `json:"state"`
	AgentOutcome    string `json:"agent_outcome"`
	RevisionID      string `json:"revision_id,omitempty"`
	VerificationID  string `json:"verification_id,omitempty"`
	CompletedEvents int    `json:"message_complete_events"`
}

// VerificationFacts is the final verification of the delivered answer.
type VerificationFacts struct {
	Result          string         `json:"result"`
	DownloadAllowed bool           `json:"download_allowed"`
	Warnings        int            `json:"warnings"`
	Blocking        int            `json:"blocking"`
	Issues          map[string]int `json:"issues,omitempty"`
	ScopeSHA256     string         `json:"scope_sha256"`
}

// CoverageFacts is the run's coverage report, without the per-piece names.
type CoverageFacts struct {
	Complete    bool                `json:"complete"`
	Exhaustive  bool                `json:"exhaustive"`
	TotalPieces int                 `json:"total_pieces"`
	FullyRead   int                 `json:"fully_read"`
	Segments    types.SegmentCounts `json:"segments"`
	States      map[string]int      `json:"states"`
	ScopeSHA256 string              `json:"scope_sha256"`
}

// RevisionFacts is one document revision of the run.
type RevisionFacts struct {
	ID            string                     `json:"id"`
	DocumentKey   string                     `json:"document_key"`
	Revision      int                        `json:"revision"`
	Kind          string                     `json:"kind"`
	ContentSHA256 string                     `json:"content_sha256"`
	Published     bool                       `json:"published"`
	Body          types.DocumentRevisionBody `json:"-"`
}

// ArtifactFacts is one published artifact and what downloading it gave.
type ArtifactFacts struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Name           string `json:"-"`
	RevisionKind   string `json:"revision_kind,omitempty"`
	RevisionID     string `json:"revision_id,omitempty"`
	Partial        bool   `json:"partial"`
	PieceWarnings  int    `json:"piece_warnings"`
	DownloadStatus int    `json:"download_status"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	Path           string `json:"path,omitempty"`
	Text           string `json:"-"` // DOCX visible text
}

// CitationFacts is one published citation.
type CitationFacts struct {
	SourceType string            `json:"source_type"`
	Label      string            `json:"-"`
	Article    string            `json:"article,omitempty"`
	PieceFile  string            `json:"piece_file,omitempty"`
	OnAnswer   bool              `json:"on_answer"`
	Checks     map[string]string `json:"checks"`
	// Locator is where the citation points, as the thread shows it.
	Locator *types.SourceLocator `json:"locator,omitempty"`
	// Corpus is a legal citation re-resolved by the eval itself against the
	// stored corpus (LegalCitationProblem): CorpusResolved, or what failed.
	// It never reads the publisher's own check statuses.
	Corpus string `json:"corpus,omitempty"`
	// Number is the [n] the text carries.
	Number int `json:"number"`
	// Identity names exactly what the citation points to — the pièce, the
	// article version, the decision, the page — and Quote is the passage
	// cited. Both are in the lawyer's review outputs, not the report.
	Identity string `json:"-"`
	Quote    string `json:"-"`
}

// CorpusResolved is a legal citation the eval re-resolved.
const CorpusResolved = "resolved"

// LegalCitationProblem re-resolves one published legal citation against the
// stored version its evidence span names (nil when none resolves): the quote
// is in that version's text and, for an article, the version was in force
// on the citation's applicability date; for a decision, the date matches.
// It returns "" when the citation holds.
func LegalCitationProblem(ci types.AgentCitation, v *types.LegalVersion) string {
	if v == nil {
		return "the cited version does not resolve in the corpus"
	}
	quote := strings.Join(strings.Fields(ci.Quote), " ")
	if quote == "" || !strings.Contains(strings.Join(strings.Fields(v.Text), " "), quote) {
		return "the quote is not in the cited version's text"
	}
	switch ci.SourceType {
	case types.CitationSourceLegifrance:
		at := ci.ApplicabilityDate
		switch {
		case at == nil:
			return "the citation records no applicability date"
		case v.ValidFrom != nil && *at < *v.ValidFrom, v.ValidTo != nil && *at >= *v.ValidTo,
			types.NormalizeLegalVersionState(v.State) == types.LegalVersionAnnulled:
			return "the cited version was not in force on its applicability date"
		}
	case types.CitationSourceJudilibre:
		if ci.DecisionDate != nil && (v.DecisionDate == nil || *v.DecisionDate != *ci.DecisionDate) {
			return "the decision date differs from the stored decision"
		}
	}
	return ""
}

// ToolFacts counts one tool's calls by final status.
type ToolFacts struct {
	Calls int `json:"calls"`
	OK    int `json:"ok"`
	Empty int `json:"empty"`
	Error int `json:"error"`
}

// QuestionFacts is one question the agent asked the lawyer.
type QuestionFacts struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Answered bool   `json:"answered"`
	Prompt   string `json:"-"` // in the reviewable outputs, not the report
}

// Failure is one unmet oracle requirement.
type Failure struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

// resendWording is an INVITATION to send the same request again — an
// imperative or a "vous pouvez …" — which is forbidden: the explanation of a
// failed turn must say what would unblock it. Saying that a resend would NOT
// help ("Renvoyer la demande à l'identique mènerait au même blocage") is the
// required wording and does not match; neither does advice to chase a party
// ("relancez l'employeur").
var resendWording = regexp.MustCompile(
	`\b(?:renvoyez|reenvoyez|relancez|resoumettez)\s+(?:votre|la|cette|sa)\s+(?:demande|requete|question|instruction)` +
		`|\b(?:veuillez|vous pouvez|n'hesitez pas a|merci de|il suffit de)\s+(?:renvoyer|reenvoyer|relancer|resoumettre)\s+(?:votre|la|cette)\s+(?:demande|requete|question|instruction)` +
		`|\b(?:reessayez|retentez)\b` +
		`|\b(?:veuillez|vous pouvez|n'hesitez pas a|merci de)\s+(?:reessayer|retenter)`)

// Evaluate judges one trial. The order is the report's: universal rules of
// the plan first, then the case's own oracle.
func Evaluate(c Case, o Outcome, m Manifest) []Failure {
	var out []Failure
	fail := func(check, format string, args ...any) {
		out = append(out, Failure{Check: check, Detail: fmt.Sprintf(format, args...)})
	}
	or := c.Oracle
	delivered := o.Outcome == types.AgentOutcomeCompleted || o.Outcome == types.AgentOutcomePartial

	// --- universal ---------------------------------------------------------
	if !slices.Contains(or.Outcomes, o.Outcome) {
		fail("outcome", "outcome %q, allowed %v (run %s, failure %q)", o.Outcome, or.Outcomes, o.RunState, o.FailureCode)
	}
	ceiling := types.MaxAgentDispatches
	if or.MaxDispatches > 0 {
		ceiling = or.MaxDispatches
	}
	if o.Dispatches > ceiling || o.ControllerCalls > types.MaxAgentDispatches {
		fail("dispatch_cap", "%d dispatches (%d controller calls recorded), ceiling %d", o.Dispatches, o.ControllerCalls, ceiling)
	}
	if or.ExactDispatches > 0 && (o.Dispatches != or.ExactDispatches || o.ControllerCalls != or.ExactDispatches) {
		fail("dispatch_cap", "%d dispatches (%d recorded), want exactly %d", o.Dispatches, o.ControllerCalls, or.ExactDispatches)
	}
	if o.Exhaustive && o.Outcome == types.AgentOutcomeCompleted && (o.Coverage == nil || !o.Coverage.Complete) {
		fail("coverage", "an exhaustive request was presented as completed without complete coverage")
	}
	if c.Competence == "" {
		for _, r := range o.Revisions {
			if r.Kind == types.AgentOutputActe {
				fail("acte_without_competence", "an acte revision exists with no compétence picked")
				break
			}
		}
	}
	for _, ci := range o.Citations {
		if ci.SourceType != types.CitationSourceLegifrance && ci.SourceType != types.CitationSourceJudilibre {
			continue
		}
		if ci.Checks[types.CitationCheckSourceResolved] != types.CitationCheckPassed {
			fail("legal_citation", "a published %s citation is not resolved (%s)", ci.SourceType, ci.Checks[types.CitationCheckSourceResolved])
		}
		if ci.Checks[types.CitationCheckTemporalValid] == types.CitationCheckFailed {
			fail("legal_citation", "a published %s citation is not valid at its date", ci.SourceType)
		}
		for _, check := range []string{types.CitationCheckQuoteMatched, types.CitationCheckOfficialMatched} {
			if ci.Checks[check] != types.CitationCheckPassed {
				fail("legal_citation", "a published %s citation has %s %q", ci.SourceType, check, ci.Checks[check])
			}
		}
		if ci.Corpus != CorpusResolved {
			fail("legal_citation", "a published %s citation does not re-resolve against the corpus: %s",
				ci.SourceType, firstNonEmpty(ci.Corpus, "not checked"))
		}
	}
	for _, ci := range o.Citations {
		if ci.SourceType != types.CitationSourcePiece {
			continue
		}
		if problem := pieceLocatorProblem(ci.PieceFile, ci.Locator); problem != "" {
			fail("piece_locator", "%s (%s)", problem, ci.PieceFile)
		}
	}
	for _, a := range o.Artifacts {
		if a.DownloadStatus != 200 {
			fail("download", "published %s artifact downloads with status %d", firstNonEmpty(a.RevisionKind, a.Kind), a.DownloadStatus)
		}
	}
	if v := o.Verification; v != nil && delivered {
		if v.Result == types.VerificationBlocked {
			fail("verification", "the delivered answer's verification is blocked")
		}
		if v.Warnings > 0 && v.Blocking == 0 && !v.DownloadAllowed {
			fail("warnings_download", "pièce warnings made the result non-downloadable")
		}
	}
	if delivered {
		if strings.TrimSpace(o.Answer.Text) == "" || o.Answer.State != types.MessageStateComplete {
			fail("delivery", "a %s turn delivered no complete answer (state %q)", o.Outcome, o.Answer.State)
		}
		if o.Answer.CompletedEvents != 1 {
			fail("delivery", "%d message.complete events, want exactly 1", o.Answer.CompletedEvents)
		}
	}
	if resendWording.MatchString(fold(o.Answer.Text)) {
		fail("futile_resend", "the delivered text invites the lawyer to send the request again")
	}

	// --- the case's oracle ---------------------------------------------------
	if or.CompleteCoverage && o.Outcome == types.AgentOutcomeCompleted && (o.Coverage == nil || !o.Coverage.Complete) {
		fail("coverage", "completed without complete coverage of the frozen scope")
	}
	if or.RequireLimitation && delivered && o.Outcome != types.AgentOutcomePartial && !answerHasLimitation(o) {
		fail("limitation", "the answer states no limitation (no limitation block, not partial)")
	}
	types_ := map[string]int{}
	for _, ci := range o.Citations {
		types_[ci.SourceType]++
	}
	if delivered {
		for _, st := range or.RequireSourceTypes {
			if types_[st] == 0 {
				fail("citations", "no published %s citation", st)
			}
		}
	}
	for _, st := range or.ForbidSourceTypes {
		if types_[st] > 0 {
			fail("citations", "%d published %s citation(s), none allowed", types_[st], st)
		}
	}
	cited := citedPieces(o)
	if delivered {
		for _, f := range or.RequirePieces {
			if !cited[f] {
				fail("pieces", "pièce %s is not cited", f)
			}
		}
	}
	if len(or.AllowedPieces) > 0 {
		for f := range cited {
			if !slices.Contains(or.AllowedPieces, f) {
				fail("explicit_subset", "pièce %s is cited outside the subset the lawyer named", f)
			}
		}
	}
	published := map[string]bool{}
	for _, a := range o.Artifacts {
		if a.DownloadStatus != 200 {
			continue
		}
		if a.RevisionKind != "" {
			published[a.RevisionKind] = true
		}
		// export_dossier publishes the controlled bordereau without an
		// agent revision.
		if a.Kind == types.ArtifactKindBordereauDocx {
			published[types.AgentOutputBordereau] = true
		}
	}
	if delivered {
		for _, k := range or.RequireDocuments {
			if !published[k] {
				fail("documents", "no downloadable %s document was published", k)
			}
		}
	}
	for _, k := range or.ForbidDocuments {
		for _, r := range o.Revisions {
			if r.Kind == k {
				fail("documents", "a %s revision was written (substitution of the deliverable)", k)
				break
			}
		}
	}
	if or.DOCXFidelity {
		out = append(out, docxFidelity(o)...)
	}
	for _, tool := range or.ForbidTools {
		if t := o.Tools[tool]; t != nil && t.Calls > 0 {
			fail("tools", "%s was called %d time(s)", tool, t.Calls)
		}
	}
	for _, tool := range or.RequireTools {
		if t := o.Tools[tool]; t == nil || t.OK == 0 {
			fail("tools", "%s never succeeded", tool)
		}
	}
	if or.ForbidQuestions && len(o.Questions) > 0 {
		fail("questions", "%d question(s) asked where none is needed", len(o.Questions))
	}
	if or.RequireQuestion && len(o.Questions) == 0 {
		fail("questions", "no question was asked")
	}
	if or.RequireAnswered && o.AnswerRounds == 0 {
		fail("answers", "the turn never parked on a question that was answered")
	}
	if or.RequireRecovery && o.ErrorRows+o.Rejections == 0 {
		fail("recovery", "no failed call was observed, so no recovery was exercised")
	}
	if or.GoldEvents != "" {
		out = append(out, goldRecall(m.Gold[or.GoldEvents], o)...)
	}
	text := fold(o.Answer.Text)
	for _, s := range or.ForbidText {
		if strings.Contains(text, fold(s)) {
			fail("forbidden_text", "the delivered text contains %q", s)
		}
	}
	if or.RequireQueryAttempt && o.Outcome != OutcomeAsked && o.RejectedQueries+len(o.PublicQueries) == 0 {
		fail("public_query", "no public query was attempted: the confidentiality boundary was never exercised")
	}
	for _, q := range append(slices.Clone(o.PublicQueries), o.ForwardedQueries...) {
		fq := fold(q)
		for _, s := range or.ForbidQueryTerms {
			if strings.Contains(fq, fold(s)) {
				fail("public_query", "a public query carries the private term %q", s)
			}
		}
	}
	for _, ci := range o.Citations {
		for _, a := range or.ForbidCitations {
			if strings.EqualFold(ci.Article, a) {
				fail("invented_law", "article %s was published as a citation", a)
			}
		}
	}
	if or.ScopeFresh && delivered {
		switch {
		case o.Verification == nil:
			fail("stale_publication", "no final verification to compare")
		case o.LiveScopeSHA256 == "" || o.Verification.ScopeSHA256 != o.LiveScopeSHA256:
			fail("stale_publication", "the published verification is bound to a scope that is no longer the dossier's")
		}
	}
	return out
}

// pieceLocatorProblem says why a pièce citation does not point where its
// file format can be pointed at: a page of a PDF, a paragraph of a DOCX, a
// cell of an XLSX — never a page of a word-processing or spreadsheet file —
// with a non-empty range. Empty when it does, or when the pièce is not a
// fixture file (a generated pièce).
func pieceLocatorProblem(file string, l *types.SourceLocator) string {
	ext := strings.ToLower(path.Ext(file))
	if strings.HasPrefix(file, "piece:") || (ext != ".pdf" && ext != ".docx" && ext != ".xlsx") {
		return ""
	}
	if l == nil {
		return "a pièce citation without a locator"
	}
	switch ext {
	case ".pdf":
		if l.Kind != types.LocatorPDF || l.Page < 1 {
			return fmt.Sprintf("a PDF pièce citation located as %q without its page", l.Kind)
		}
	case ".docx":
		if l.Kind != types.LocatorDOCX || l.Paragraph < 1 || l.Page != 0 {
			return fmt.Sprintf("a DOCX pièce citation located as %q, not by paragraph", l.Kind)
		}
	case ".xlsx":
		if l.Kind != types.LocatorXLSX || l.Sheet == "" || l.Cell == "" || l.Page != 0 {
			return fmt.Sprintf("an XLSX pièce citation located as %q, not by sheet and cell", l.Kind)
		}
	}
	if l.End <= l.Start {
		return "a pièce citation with an empty range"
	}
	return ""
}

// answerHasLimitation: the delivered answer revision has a limitation block.
func answerHasLimitation(o Outcome) bool {
	for _, r := range o.Revisions {
		if r.ID != o.Answer.RevisionID {
			continue
		}
		for _, b := range r.Body.Blocks {
			if b.Role == types.BlockLimitation && strings.TrimSpace(b.Text) != "" {
				return true
			}
		}
	}
	return false
}

// citedPieces is every fixture file a published citation or a published
// revision's claim points at.
func citedPieces(o Outcome) map[string]bool {
	out := map[string]bool{}
	for _, ci := range o.Citations {
		if ci.PieceFile != "" {
			out[ci.PieceFile] = true
		}
	}
	for _, r := range o.Revisions {
		if !r.Published {
			continue
		}
		for _, b := range r.Body.Blocks {
			for _, cl := range b.Claims {
				for _, id := range cl.EvidenceIDs {
					if f := o.EvidencePieces[id]; f != "" {
						out[f] = true
					}
				}
			}
		}
	}
	return out
}

// docxFidelity checks the downloaded file against the verified revision it
// was rendered from: every chronology row's text is in the DOCX, and a
// document published with pièce warnings carries « À vérifier » inside the
// file itself, not only in the UI.
func docxFidelity(o Outcome) []Failure {
	var out []Failure
	revs := map[string]RevisionFacts{}
	for _, r := range o.Revisions {
		revs[r.ID] = r
	}
	for _, a := range o.Artifacts {
		r, ok := revs[a.RevisionID]
		if !ok || a.Text == "" {
			if ok && a.DownloadStatus == 200 {
				out = append(out, Failure{Check: "docx_fidelity", Detail: fmt.Sprintf("the %s DOCX has no readable text", r.Kind)})
			}
			continue
		}
		docText := squash(fold(a.Text))
		missing := 0
		for _, b := range r.Body.Blocks {
			if b.Role != types.BlockChronologyRow {
				continue
			}
			if !strings.Contains(docText, squash(fold(b.Text))) {
				missing++
			}
		}
		if missing > 0 {
			out = append(out, Failure{Check: "docx_fidelity", Detail: fmt.Sprintf("%d verified chronology row(s) are not in the %s DOCX", missing, r.Kind)})
		}
		if a.PieceWarnings > 0 && !strings.Contains(docText, "averifier") { // « À vérifier », squashed
			out = append(out, Failure{Check: "docx_fidelity", Detail: "the DOCX carries pièce warnings in the UI only, not « À vérifier » in the file"})
		}
	}
	return out
}

// goldRecall checks every lawyer-labelled material event: included at its
// own event date with one of its pièces, or explicitly left unresolved in a
// limitation or warning block. A row only at another date — the document
// date of a pièce that relates the event — does not count: that is the
// forbidden substitution.
func goldRecall(g GoldList, o Outcome) []Failure {
	missed := goldMissed(g, o)
	if len(missed) == 0 {
		return nil
	}
	return []Failure{{Check: "material_events", Detail: fmt.Sprintf("%d/%d gold events neither included nor unresolved: %s",
		len(missed), len(g.Events), strings.Join(missed, ", "))}}
}

// GoldRecall is the share of a gold list's events the outcome covers.
func GoldRecall(g GoldList, o Outcome) float64 {
	if len(g.Events) == 0 {
		return 1
	}
	return float64(len(g.Events)-len(goldMissed(g, o))) / float64(len(g.Events))
}

// goldMissed lists the gold events the outcome neither includes nor leaves
// explicitly unresolved.
func goldMissed(g GoldList, o Outcome) []string {
	type blk struct {
		role   string
		text   string
		dates  []types.PartialDate
		pieces map[string]bool
	}
	var blocks []blk
	for _, r := range o.Revisions {
		if !r.Published {
			continue
		}
		for _, b := range r.Body.Blocks {
			x := blk{role: b.Role, text: fold(b.Text), pieces: map[string]bool{}}
			// Every date role counts: what matters is the date VALUE. An
			// avertissement, a convocation or a solde de tout compte IS its
			// document, so its row is rightly labelled with the document (or
			// receipt) date — and that date is the event's. A substitution is
			// an event shown only at another date (the contract's signature
			// instead of the embauche), and that never equals the gold date.
			if b.Chronology != nil {
				x.dates = append(x.dates, b.Chronology.Date.PartialDate)
			}
			for _, cl := range b.Claims {
				for _, d := range cl.Dates {
					x.dates = append(x.dates, d.PartialDate)
				}
				for _, id := range cl.EvidenceIDs {
					if f := o.EvidencePieces[id]; f != "" {
						x.pieces[f] = true
					}
				}
			}
			blocks = append(blocks, x)
		}
	}
	var missed []string
	for _, e := range g.Events {
		want, err := parseGoldDate(e.Date)
		if err != nil {
			missed = append(missed, e.ID+" (unparseable date)")
			continue
		}
		found := false
		for _, b := range blocks {
			if !anyPiece(b.pieces, e.Pieces) {
				continue
			}
			for _, d := range b.dates {
				if dateMatches(d, want) {
					found = true
				}
			}
		}
		if !found {
			// Explicitly unresolved: a limitation or warning that cites one
			// of the event's pièces and names its date — never a bare
			// keyword, which a generic disclaimer (« ne qualifie pas le
			// licenciement ») would satisfy for events it never placed.
			for _, b := range blocks {
				if (b.role == types.BlockLimitation || b.role == types.BlockWarning) &&
					anyPiece(b.pieces, e.Pieces) && textNamesDate(b.text, want) {
					found = true
				}
			}
		}
		if !found {
			missed = append(missed, e.ID+" ("+e.Date+")")
		}
	}
	sort.Strings(missed)
	return missed
}

func dateMatches(d types.PartialDate, want goldDate) bool {
	if d.Year != want.Year {
		return false
	}
	switch want.Precision {
	case types.DatePrecisionYear:
		return true
	case types.DatePrecisionMonth:
		return d.Month == want.Month
	default:
		return d.Month == want.Month && d.Day == want.Day
	}
}

var frenchMonths = [...]string{"janvier", "fevrier", "mars", "avril", "mai", "juin", "juillet", "aout",
	"septembre", "octobre", "novembre", "decembre"}

// textNamesDate reports whether folded text names the date at its own
// precision: « 7 avril 2025 », « 07/04/2025 », « 2025-04-07 »; « avril
// 2025 » for a month; the year for a year.
func textNamesDate(text string, want goldDate) bool {
	y := strconv.Itoa(want.Year)
	forms := []string{y}
	if want.Precision != types.DatePrecisionYear {
		m := frenchMonths[want.Month-1]
		forms = []string{m + " " + y, fmt.Sprintf("%02d/%s", want.Month, y), fmt.Sprintf("%s-%02d", y, want.Month)}
		if want.Precision == types.DatePrecisionDay {
			d := strconv.Itoa(want.Day)
			forms = []string{d + " " + m + " " + y, fmt.Sprintf("%02d %s %s", want.Day, m, y),
				fmt.Sprintf("%02d/%02d/%s", want.Day, want.Month, y), fmt.Sprintf("%s-%02d-%02d", y, want.Month, want.Day)}
			if want.Day == 1 {
				forms = append(forms, "1er "+m+" "+y)
			}
		}
	}
	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }
	for _, f := range forms {
		for from := 0; ; {
			i := strings.Index(text[from:], f)
			if i < 0 {
				break
			}
			i += from
			end := i + len(f)
			if (i == 0 || !isDigit(text[i-1])) && (end == len(text) || !isDigit(text[end])) {
				return true
			}
			from = i + 1
		}
	}
	return false
}

func anyPiece(have map[string]bool, want []string) bool {
	for _, w := range want {
		if have[w] {
			return true
		}
	}
	return false
}

// fold lowercases, strips French diacritics and normalises the typographic
// characters pandoc and the models use, so comparisons are about words.
func fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'à', 'â', 'ä', 'á':
			r = 'a'
		case 'ç':
			r = 'c'
		case 'é', 'è', 'ê', 'ë':
			r = 'e'
		case 'î', 'ï', 'í':
			r = 'i'
		case 'ô', 'ö', 'ó':
			r = 'o'
		case 'ù', 'û', 'ü', 'ú':
			r = 'u'
		case 'ÿ':
			r = 'y'
		case 'œ':
			b.WriteString("oe")
			continue
		case 'æ':
			b.WriteString("ae")
			continue
		case '’', 'ʼ', '‘', '`':
			r = '\''
		case '“', '”', '«', '»':
			r = '"'
		case '‐', '‑', '‒', '–', '—':
			r = '-'
		case ' ', ' ', ' ':
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}

// squash keeps letters and digits only: rendering may re-break lines, change
// quotes or drop markup, none of which changes what a row says.
func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
