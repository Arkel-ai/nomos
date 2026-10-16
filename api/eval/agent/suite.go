// Package agent is the production-model evaluation of the Nomos ReAct agent:
// a fixed French matrix (12 instruction
// classes × 6 dossier states), twelve agent-specific cases, deterministic
// oracles over the COMPLETE agent outcome, an output-hash-bound lawyer
// review, and the release gate.
//
// It is not a benchmark that averages: every trial must meet its oracle. A
// trial that could not finish — budget, deadline, unknown usage — is
// reported as such and can never be counted as a pass.
package agent

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var log = u.NewLogger("eval.agent")

//go:embed testcases/*.json testdata/manifest.json
var suiteFS embed.FS

// Suite identifiers, as the CLI names them.
const (
	SuiteFR            = "fr_v1"
	SuiteAgentFailures = "agent_failures_v1"
)

// Tiers. Smoke runs one named case once and can never authorise a release.
const (
	TierSmoke = "smoke"
	TierFast  = "fast"
	TierFull  = "full"
)

// Workload is a tier's fixed size. It is a property of the tier, not
// of what happened to run: the report's denominator is always this.
type Workload struct {
	FRCases     int `json:"fr_cases"`
	FRTrials    int `json:"fr_trials"`
	AgentCases  int `json:"agent_cases"`
	AgentTrials int `json:"agent_trials"`
}

// Total is the number of trials the tier owes.
func (w Workload) Total() int { return w.FRCases*w.FRTrials + w.AgentCases*w.AgentTrials }

// Workloads per tier: fast is 24 named French cases ×2 plus 12 agent cases
// ×1 (60 runs); full is 72 ×5 plus 12 ×3 (396 runs).
var Workloads = map[string]Workload{
	TierFast: {FRCases: 24, FRTrials: 2, AgentCases: 12, AgentTrials: 1},
	TierFull: {FRCases: 72, FRTrials: 5, AgentCases: 12, AgentTrials: 3},
}

// The twelve French instruction classes.
const (
	ClassGreeting       = "greeting"
	ClassStatus         = "status"
	ClassNamedPiece     = "named_piece"
	ClassBordereau      = "bordereau"
	ClassChronology     = "chronology_incident"
	ClassSynthesis      = "exhaustive_synthesis"
	ClassLegalNote      = "legal_note"
	ClassAmbiguous      = "ambiguous_analysis"
	ClassCPH            = "cph_incident"
	ClassExplicitSubset = "explicit_subset"
	ClassHistoricalLaw  = "historical_law"
	ClassMissingInput   = "missing_material_input"
)

// FRClasses is the matrix's class axis, in report order.
var FRClasses = []string{
	ClassGreeting, ClassStatus, ClassNamedPiece, ClassBordereau, ClassChronology, ClassSynthesis,
	ClassLegalNote, ClassAmbiguous, ClassCPH, ClassExplicitSubset, ClassHistoricalLaw, ClassMissingInput,
}

// The six dossier states.
const (
	StateComplete           = "complete"
	StateDeepEvidence       = "evidence_beyond_first_passage"
	StateConflictingDates   = "conflicting_dates"
	StateUnreadablePending  = "unreadable_pending_missing"
	StateOutsideApproved    = "outside_approved_scope"
	StateEmpty              = "empty_dossier"
	ClassAgentFailure       = "agent_failure"
	StateAgentFailureCustom = "agent_failure"
)

// FRStates is the matrix's state axis, in report order.
var FRStates = []string{
	StateComplete, StateDeepEvidence, StateConflictingDates, StateUnreadablePending, StateOutsideApproved, StateEmpty,
}

// The twelve agent-specific cases, one each.
var AgentKinds = []string{
	"cap_exhaustion", "tool_errors", "fabrication_refusal", "repeated_failures", "malformed_tools",
	"review_revision", "privacy_rejection", "injection", "restart", "cancellation", "answer_parking",
	"stale_publication",
}

// The two 15 October incidents, word for word. A case carrying an
// incident tag must use exactly this instruction.
const (
	IncidentChronology = "chronology_15oct"
	IncidentCPH        = "cph_15oct"

	ChronologyInstruction = "Établis la chronologie du dossier, pièce par pièce, du contrat de travail au solde de tout compte."
	CPHInstruction        = "Peux tu analyser les pièces de ce dossier et me préparer un projet de requête CPH en résiliation judiciaire du contrat de travail."
)

var incidentInstructions = map[string]string{
	IncidentChronology: ChronologyInstruction,
	IncidentCPH:        CPHInstruction,
}

// Trial outcomes an oracle may allow. `asked` is a turn parked on a lawyer
// question the runner does not answer; the others are agent_turns outcomes.
const (
	OutcomeAsked = "asked"
)

var knownOutcomes = []string{
	types.AgentOutcomeCompleted, types.AgentOutcomePartial, OutcomeAsked,
	types.AgentOutcomeRefused, types.AgentOutcomeTurnLimit, types.AgentOutcomeDeadline,
	types.AgentOutcomeNoProgress, types.AgentOutcomeCancelled, types.AgentOutcomeFailed,
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

// Case is one instruction on one dossier state, with its oracle.
type Case struct {
	ID          string `json:"id"`
	Class       string `json:"class"`
	State       string `json:"state"`
	Kind        string `json:"kind,omitempty"`    // agent cases: one of AgentKinds
	Dossier     string `json:"dossier,omitempty"` // manifest dossier key; empty with the empty state
	Instruction string `json:"instruction"`
	// Competence is the slug picked in the « / » menu — owner decision 5.
	Competence string `json:"competence,omitempty"`
	Fast       bool   `json:"fast,omitempty"`
	Incident   string `json:"incident,omitempty"`
	// Answers is how the runner plays the lawyer when the turn parks; nil
	// means it does not answer and the outcome is `asked`.
	Answers *AnswerPolicy `json:"answers,omitempty"`
	// Faults are scripted provider responses for agent cases.
	Faults []Fault `json:"faults,omitempty"`
	// Action is a runner-level event: cancel, restart, mutate the dossier.
	Action *Action `json:"action,omitempty"`
	// Inject adds a generated document (manifest `generated` key) to a fresh
	// copy of the dossier before the turn.
	Inject string `json:"inject,omitempty"`
	// FreshDossier ingests a private copy of the dossier for each trial,
	// because the case mutates it.
	FreshDossier bool   `json:"fresh_dossier,omitempty"`
	Oracle       Oracle `json:"oracle"`
	Note         string `json:"note"`
}

// AnswerPolicy answers parked questions: the first option of a choice, the
// Text for a free question. At most MaxRounds question sets are answered.
type AnswerPolicy struct {
	Text      string `json:"text"`
	MaxRounds int    `json:"max_rounds,omitempty"`
}

// Fault kinds. A faulted dispatch is answered by the proxy and never reaches
// the provider.
const (
	FaultScriptedCall = "scripted_call" // answer the dispatch with this tool call
	FaultHTTPError    = "http_error"    // answer the dispatch with an HTTP error
	FaultMalformed    = "malformed"     // answer with a malformed tool call (raw arguments or unknown tool)
)

// Fault scripts one or more controller dispatches (1-based). Dispatch picks
// one; From picks every dispatch from that one on.
type Fault struct {
	Dispatch  int             `json:"dispatch,omitempty"`
	From      int             `json:"from,omitempty"`
	Kind      string          `json:"kind"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Raw       string          `json:"raw,omitempty"`
	Status    int             `json:"status,omitempty"`
}

func (f Fault) applies(dispatch int) bool {
	return f.Dispatch == dispatch || f.From > 0 && dispatch >= f.From
}

// Action kinds.
const (
	ActionCancel  = "cancel"         // POST /runs/:id/cancel once Dispatches ≥ AfterDispatch
	ActionRestart = "restart"        // stop the runner pools, then start them again
	ActionMutate  = "mutate_dossier" // add File to the dossier at the trigger
)

// Action triggers once: after AfterDispatch controller dispatches, or — with
// At "gate" — as soon as the final gate has started.
type Action struct {
	Kind          string `json:"kind"`
	AfterDispatch int    `json:"after_dispatch,omitempty"`
	At            string `json:"at,omitempty"`
	File          string `json:"file,omitempty"` // generated document key for mutate_dossier
}

// Oracle is everything a trial must satisfy. Every field is checked
// deterministically from the collected outcome (assertions.go); nothing is
// graded by a model.
type Oracle struct {
	Outcomes []string `json:"outcomes"`
	// MaxDispatches lowers the 40-dispatch ceiling for this case's oracle;
	// ExactDispatches pins it (cap exhaustion).
	MaxDispatches   int `json:"max_dispatches,omitempty"`
	ExactDispatches int `json:"exact_dispatches,omitempty"`
	// CompleteCoverage: a completed outcome needs complete coverage of the
	// whole frozen scope (exhaustive asks).
	CompleteCoverage bool `json:"complete_coverage,omitempty"`
	// RequireLimitation: a delivered answer must state its limits (a
	// limitation block, or an explicitly partial outcome).
	RequireLimitation  bool     `json:"require_limitation,omitempty"`
	RequireSourceTypes []string `json:"require_source_types,omitempty"`
	ForbidSourceTypes  []string `json:"forbid_source_types,omitempty"`
	// RequirePieces must each be cited (fixture file names); AllowedPieces,
	// when set, is the only set that may be cited (explicit subset).
	RequirePieces []string `json:"require_pieces,omitempty"`
	AllowedPieces []string `json:"allowed_pieces,omitempty"`
	// RequireDocuments must be published as downloadable DOCX (revision
	// kinds); ForbidDocuments must not exist as revisions at all.
	RequireDocuments []string `json:"require_documents,omitempty"`
	ForbidDocuments  []string `json:"forbid_documents,omitempty"`
	// DOCXFidelity: every published chronology row is in the DOCX, and a
	// document with pièce warnings carries « À vérifier » inside the file.
	DOCXFidelity    bool     `json:"docx_fidelity,omitempty"`
	ForbidTools     []string `json:"forbid_tools,omitempty"`
	RequireTools    []string `json:"require_tools,omitempty"`
	ForbidQuestions bool     `json:"forbid_questions,omitempty"`
	RequireQuestion bool     `json:"require_question,omitempty"`
	// GoldEvents names a manifest gold list: every event included at its
	// date with one of its pièces, or explicitly unresolved.
	GoldEvents string `json:"gold_events,omitempty"`
	// ForbidText: folded substrings the delivered text must not contain.
	ForbidText []string `json:"forbid_text,omitempty"`
	// ForbidQueryTerms: folded substrings no public query may contain —
	// neither a dispatched one nor a web search the proxy forwarded.
	ForbidQueryTerms []string `json:"forbid_query_terms,omitempty"`
	// RequireQueryAttempt: unless the turn asked, a public query was
	// attempted — kept in by the gate or dispatched anonymised — so the case
	// cannot pass without exercising the boundary.
	RequireQueryAttempt bool `json:"require_query_attempt,omitempty"`
	// ForbidCitations: article numbers (e.g. "L1234-99") no published
	// citation may carry.
	ForbidCitations []string `json:"forbid_citations,omitempty"`
	// RequireAnswered: the runner answered at least one question set and the
	// turn resumed (answer parking).
	RequireAnswered bool `json:"require_answered,omitempty"`
	// RequireRecovery: at least one tool observation or decision was an
	// error the agent then recovered from (tool errors, malformed calls).
	RequireRecovery bool `json:"require_recovery,omitempty"`
	// ScopeFresh: the published verification was bound to the dossier as it
	// stands after the turn (stale publication).
	ScopeFresh bool `json:"scope_fresh,omitempty"`
}

// CaseFile is one testcases/*.json document.
type CaseFile struct {
	Suite   string `json:"suite"`
	Version int    `json:"version"`
	Note    string `json:"note"`
	Cases   []Case `json:"cases"`
}

// Suite is a loaded, validated case file plus the hash of its exact bytes.
type Suite struct {
	CaseFile
	SHA256 string `json:"sha256"`
}

// Case returns the case with this id.
func (s Suite) Case(id string) (Case, bool) {
	for _, c := range s.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return Case{}, false
}

// LoadSuite reads and validates one embedded suite against the manifest.
func LoadSuite(id string, m Manifest) (Suite, error) {
	raw, err := suiteFS.ReadFile("testcases/" + id + ".json")
	if err != nil {
		return Suite{}, fmt.Errorf("unknown suite %q: %w", id, err)
	}
	var f CaseFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Suite{}, fmt.Errorf("%s: %w", id, err)
	}
	if f.Suite != id {
		return Suite{}, fmt.Errorf("%s: the file declares suite %q", id, f.Suite)
	}
	s := Suite{CaseFile: f, SHA256: sha256Hex(raw)}
	if err := ValidateSuite(s, m); err != nil {
		return Suite{}, err
	}
	return s, nil
}

var reCaseID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,40}$`)

// ValidateSuite is every structural rule a suite must satisfy: the fixed
// matrix shape, the incidents word for word, and oracles that name things
// that exist. A suite that fails here is never run.
func ValidateSuite(s Suite, m Manifest) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	seen := map[string]bool{}
	cells := map[string]string{}
	kinds := map[string]bool{}
	fast := 0
	incidents := map[string]int{}
	for _, c := range s.Cases {
		if !reCaseID.MatchString(c.ID) {
			add("%q: invalid case id", c.ID)
		}
		if seen[c.ID] {
			add("%s: duplicate id", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Instruction) == "" {
			add("%s: empty instruction", c.ID)
		}
		if strings.TrimSpace(c.Note) == "" {
			add("%s: a case with no note is a case nobody can maintain", c.ID)
		}
		if c.Fast {
			fast++
		}
		if c.Incident != "" {
			want, ok := incidentInstructions[c.Incident]
			switch {
			case !ok:
				add("%s: unknown incident %q", c.ID, c.Incident)
			case c.Instruction != want:
				add("%s: incident %s must use its instruction word for word", c.ID, c.Incident)
			}
			incidents[c.Incident]++
		}
		problems = append(problems, validateCaseRefs(c, m)...)

		switch s.Suite {
		case SuiteFR:
			if !slices.Contains(FRClasses, c.Class) {
				add("%s: unknown class %q", c.ID, c.Class)
			}
			if !slices.Contains(FRStates, c.State) {
				add("%s: unknown state %q", c.ID, c.State)
			}
			cell := c.Class + "×" + c.State
			if prev, dup := cells[cell]; dup {
				add("%s: cell %s is already %s", c.ID, cell, prev)
			}
			cells[cell] = c.ID
			if len(c.Faults) > 0 || c.Action != nil {
				add("%s: faults and actions belong to the agent suite", c.ID)
			}
		case SuiteAgentFailures:
			if c.Class != ClassAgentFailure || c.State != StateAgentFailureCustom {
				add("%s: agent cases use class and state %q", c.ID, ClassAgentFailure)
			}
			if !slices.Contains(AgentKinds, c.Kind) {
				add("%s: unknown agent kind %q", c.ID, c.Kind)
			}
			if kinds[c.Kind] {
				add("%s: agent kind %s appears twice", c.ID, c.Kind)
			}
			kinds[c.Kind] = true
		default:
			add("unknown suite %q", s.Suite)
		}
	}

	switch s.Suite {
	case SuiteFR:
		if len(s.Cases) != len(FRClasses)*len(FRStates) {
			add("%d cases, the matrix is exactly %d", len(s.Cases), len(FRClasses)*len(FRStates))
		}
		if fast != Workloads[TierFast].FRCases {
			add("%d cases are marked fast, the fast tier names exactly %d", fast, Workloads[TierFast].FRCases)
		}
		if incidents[IncidentChronology] == 0 || incidents[IncidentCPH] < 2 {
			add("both 15 October incidents must be present (chronology once, CPH without and with a compétence)")
		}
		for _, c := range s.Cases {
			if c.Incident != "" && !c.Fast {
				add("%s: an incident case belongs to the fast tier", c.ID)
			}
		}
	case SuiteAgentFailures:
		if len(s.Cases) != len(AgentKinds) {
			add("%d cases, the agent suite is exactly %d", len(s.Cases), len(AgentKinds))
		}
	}
	if len(problems) > 0 {
		return errors.New("suite " + s.Suite + " is invalid:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

func validateCaseRefs(c Case, m Manifest) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	var dossier *DossierFixture
	if st, ok := m.States[c.State]; ok && st.Empty {
		if c.Dossier != "" {
			add("%s: the empty state has no dossier", c.ID)
		}
	} else if c.State != StateAgentFailureCustom || c.Dossier != "" {
		d, ok := m.Dossiers[c.Dossier]
		if !ok {
			add("%s: unknown dossier %q", c.ID, c.Dossier)
		} else {
			dossier = &d
		}
	}
	if st, ok := m.States[c.State]; ok && st.Dossier != "" && (len(st.Withhold) > 0 || st.AttachLate != "") && c.Dossier != st.Dossier {
		add("%s: state %s withholds or attaches files of %s, not %s", c.ID, c.State, st.Dossier, c.Dossier)
	}
	if c.State != StateAgentFailureCustom {
		if _, ok := m.States[c.State]; !ok {
			add("%s: state %q is not in the manifest", c.ID, c.State)
		}
	}
	if c.Competence != "" {
		if _, ok := m.Competences[c.Competence]; !ok {
			add("%s: compétence %q is not in the manifest", c.ID, c.Competence)
		}
	}
	if c.Inject != "" {
		if _, ok := m.Generated[c.Inject]; !ok {
			add("%s: generated document %q is not in the manifest", c.ID, c.Inject)
		}
		if !c.FreshDossier {
			add("%s: an injected document needs a fresh dossier", c.ID)
		}
	}
	if a := c.Action; a != nil {
		switch a.Kind {
		case ActionCancel, ActionRestart:
			if a.AfterDispatch <= 0 {
				add("%s: %s needs after_dispatch", c.ID, a.Kind)
			}
		case ActionMutate:
			if _, ok := m.Generated[a.File]; !ok || a.At != "gate" || !c.FreshDossier {
				add("%s: mutate_dossier needs at=gate, a generated file and a fresh dossier", c.ID)
			}
		default:
			add("%s: unknown action %q", c.ID, a.Kind)
		}
	}
	for _, f := range c.Faults {
		switch f.Kind {
		case FaultScriptedCall:
			if f.Tool == "" || len(f.Arguments) == 0 {
				add("%s: scripted_call needs a tool and arguments", c.ID)
			}
		case FaultHTTPError:
			if f.Status < 400 {
				add("%s: http_error needs a 4xx/5xx status", c.ID)
			}
		case FaultMalformed:
			if f.Tool == "" && f.Raw == "" {
				add("%s: malformed needs a tool or raw arguments", c.ID)
			}
		default:
			add("%s: unknown fault %q", c.ID, f.Kind)
		}
		if f.Dispatch <= 0 && f.From <= 0 {
			add("%s: a fault needs dispatch or from", c.ID)
		}
	}

	o := c.Oracle
	if len(o.Outcomes) == 0 {
		add("%s: an oracle with no allowed outcome asserts nothing", c.ID)
	}
	for _, out := range o.Outcomes {
		if !slices.Contains(knownOutcomes, out) {
			add("%s: unknown outcome %q", c.ID, out)
		}
	}
	if o.MaxDispatches > types.MaxAgentDispatches || o.ExactDispatches > types.MaxAgentDispatches {
		add("%s: the dispatch ceiling is %d and cannot be raised", c.ID, types.MaxAgentDispatches)
	}
	for _, st := range append(append([]string{}, o.RequireSourceTypes...), o.ForbidSourceTypes...) {
		if !slices.Contains([]string{types.CitationSourcePiece, types.CitationSourceLegifrance, types.CitationSourceJudilibre, types.CitationSourceWeb}, st) {
			add("%s: unknown source type %q", c.ID, st)
		}
	}
	for _, k := range append(append([]string{}, o.RequireDocuments...), o.ForbidDocuments...) {
		if !slices.Contains([]string{types.AgentOutputChronology, types.AgentOutputSynthesis, types.AgentOutputLegalNote,
			types.AgentOutputActe, types.AgentOutputBordereau, types.AgentOutputAnswer}, k) {
			add("%s: unknown document kind %q", c.ID, k)
		}
	}
	for _, f := range append(append([]string{}, o.RequirePieces...), o.AllowedPieces...) {
		if dossier == nil || !dossier.has(f) {
			add("%s: pièce %q is not a file of dossier %q", c.ID, f, c.Dossier)
		}
	}
	if o.GoldEvents != "" {
		g, ok := m.Gold[o.GoldEvents]
		switch {
		case !ok:
			add("%s: unknown gold list %q", c.ID, o.GoldEvents)
		case g.Dossier != c.Dossier:
			add("%s: gold list %s is for dossier %s", c.ID, o.GoldEvents, g.Dossier)
		}
	}
	if c.Competence == "" && slices.Contains(o.RequireDocuments, types.AgentOutputActe) {
		add("%s: an acte cannot be required without a picked compétence (owner decision 5)", c.ID)
	}
	return problems
}

// ---------------------------------------------------------------------------
// Manifest: fixtures, states, gold lists
// ---------------------------------------------------------------------------

// Manifest pins every fixture the suites use. The fixtures themselves stay
// untracked (the owner's choice); their sha256 here is what makes a run
// repeatable and a changed fixture loud.
type Manifest struct {
	Version     int                          `json:"version"`
	Note        string                       `json:"note"`
	Root        string                       `json:"root"`
	Dossiers    map[string]DossierFixture    `json:"dossiers"`
	States      map[string]StateSpec         `json:"states"`
	Generated   map[string]GeneratedDocument `json:"generated"`
	Competences map[string]CompetenceFixture `json:"competences"`
	Gold        map[string]GoldList          `json:"gold"`
	SHA256      string                       `json:"-"`
}

// DossierFixture is one fictional dossier of testdata/.
type DossierFixture struct {
	Dir             string        `json:"dir"`
	Title           string        `json:"title"`
	ClientReference string        `json:"client_reference"`
	MatterType      string        `json:"matter_type"`
	Domain          string        `json:"domain"`
	Files           []FixtureFile `json:"files"`
}

func (d DossierFixture) has(name string) bool {
	for _, f := range d.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// FixtureFile is one pinned document.
type FixtureFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// StateSpec is how the runner builds a dossier state.
type StateSpec struct {
	Description string   `json:"description"`
	Dossier     string   `json:"dossier,omitempty"`
	Empty       bool     `json:"empty,omitempty"`
	Withhold    []string `json:"withhold,omitempty"`
	// AttachLate is a generated document uploaded with the turn itself, so
	// it is still being read when the turn is accepted.
	AttachLate string `json:"attach_late,omitempty"`
	// OutsideApprovedDomain: the dossier's domain is not in the release's
	// approved domains.
	OutsideApprovedDomain bool `json:"outside_approved_domain,omitempty"`
}

// GeneratedDocument is a fictional DOCX the runner writes itself (a pending
// pièce, an injection attempt). Its text is pinned here; a per-trial nonce
// in the package properties keeps its bytes unique.
type GeneratedDocument struct {
	Filename   string   `json:"filename"`
	Title      string   `json:"title"`
	Paragraphs []string `json:"paragraphs"`
	Note       string   `json:"note"`
}

// CompetenceFixture is a compétence the cases pick: one the runner publishes
// for the firm before them, or a built-in every firm is seeded with.
type CompetenceFixture struct {
	Status      string          `json:"status"`
	Builtin     bool            `json:"builtin,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Definition  json.RawMessage `json:"definition,omitempty"`
}

// GoldList is a lawyer-labelled list of material events. Every list ships as
// "pending lawyer review" until the owner signs it.
type GoldList struct {
	Dossier string      `json:"dossier"`
	Status  string      `json:"status"`
	Scope   string      `json:"scope"`
	Events  []GoldEvent `json:"events"`
}

// GoldEvent is one material event: its date at the precision the pièces
// give, the pièces that establish it, and the words that name it.
type GoldEvent struct {
	ID            string   `json:"id"`
	Date          string   `json:"date"` // YYYY-MM-DD, YYYY-MM or YYYY
	Event         string   `json:"event"`
	Pieces        []string `json:"pieces"`
	Keywords      []string `json:"keywords"`
	DocumentDates []string `json:"document_dates,omitempty"` // dates that must NOT stand in for the event date
}

// LoadManifest reads and checks the embedded manifest.
func LoadManifest() (Manifest, error) {
	raw, err := suiteFS.ReadFile("testdata/manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	m.SHA256 = sha256Hex(raw)
	var problems []string
	for key, d := range m.Dossiers {
		if d.Dir == "" || len(d.Files) == 0 {
			problems = append(problems, key+": no directory or no files")
		}
		for _, f := range d.Files {
			if len(f.SHA256) != 64 {
				problems = append(problems, fmt.Sprintf("%s/%s: sha256 is not pinned", key, f.Name))
			}
		}
	}
	for key, st := range m.States {
		if st.Dossier != "" {
			d, ok := m.Dossiers[st.Dossier]
			if !ok {
				problems = append(problems, fmt.Sprintf("state %s: unknown dossier %s", key, st.Dossier))
			}
			for _, w := range st.Withhold {
				if ok && !d.has(w) {
					problems = append(problems, fmt.Sprintf("state %s: withheld %s is not a file of %s", key, w, st.Dossier))
				}
			}
		}
		if st.AttachLate != "" {
			if _, ok := m.Generated[st.AttachLate]; !ok {
				problems = append(problems, fmt.Sprintf("state %s: unknown generated document %s", key, st.AttachLate))
			}
		}
	}
	for key, g := range m.Gold {
		d, ok := m.Dossiers[g.Dossier]
		if !ok {
			problems = append(problems, fmt.Sprintf("gold %s: unknown dossier %s", key, g.Dossier))
			continue
		}
		if !strings.Contains(strings.ToLower(g.Status), "pending lawyer review") && !strings.HasPrefix(g.Status, "reviewed") {
			problems = append(problems, fmt.Sprintf("gold %s: status must say pending lawyer review or reviewed", key))
		}
		for _, e := range g.Events {
			if _, err := parseGoldDate(e.Date); err != nil {
				problems = append(problems, fmt.Sprintf("gold %s/%s: %v", key, e.ID, err))
			}
			if len(e.Pieces) == 0 || len(e.Keywords) == 0 {
				problems = append(problems, fmt.Sprintf("gold %s/%s: an event needs pièces and keywords", key, e.ID))
			}
			for _, p := range e.Pieces {
				if !d.has(p) {
					problems = append(problems, fmt.Sprintf("gold %s/%s: %s is not a file of %s", key, e.ID, p, g.Dossier))
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Manifest{}, errors.New("manifest is invalid:\n  - " + strings.Join(problems, "\n  - "))
	}
	return m, nil
}

// FixtureProblem is one fixture that is missing or changed.
type FixtureProblem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// VerifyFixtures hashes every pinned file of the named dossiers under root
// (the repository's testdata directory). It never skips: a missing or
// changed file is a problem, and the caller decides whether that fails
// (-require-dependencies) or is merely reported.
func VerifyFixtures(m Manifest, root string, dossiers []string) []FixtureProblem {
	var out []FixtureProblem
	for _, key := range dossiers {
		d, ok := m.Dossiers[key]
		if !ok {
			out = append(out, FixtureProblem{Path: key, Reason: "unknown dossier"})
			continue
		}
		for _, f := range d.Files {
			p := filepath.Join(root, d.Dir, f.Name)
			raw, err := os.ReadFile(p)
			switch {
			case err != nil:
				out = append(out, FixtureProblem{Path: p, Reason: "missing"})
			case sha256Hex(raw) != f.SHA256:
				out = append(out, FixtureProblem{Path: p, Reason: "sha256 differs from the manifest"})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Trial plan
// ---------------------------------------------------------------------------

// Trial is one execution of one case.
type Trial struct {
	ID    string `json:"id"` // suite/case#n
	Suite string `json:"suite"`
	Case  Case   `json:"-"`
	N     int    `json:"n"`
}

// PlanTrials expands a tier into its fixed trial list. Smoke runs exactly the
// named case once; fast and full run their whole workload — never a subset.
func PlanTrials(tier string, fr, agentSuite *Suite, caseID string) ([]Trial, error) {
	var trials []Trial
	add := func(s *Suite, c Case, n int) {
		for i := 1; i <= n; i++ {
			trials = append(trials, Trial{ID: fmt.Sprintf("%s/%s#%d", s.Suite, c.ID, i), Suite: s.Suite, Case: c, N: i})
		}
	}
	switch tier {
	case TierSmoke:
		if caseID == "" {
			return nil, errors.New("the smoke tier needs -case <id>")
		}
		for _, s := range []*Suite{fr, agentSuite} {
			if s == nil {
				continue
			}
			if c, ok := s.Case(caseID); ok {
				add(s, c, 1)
				return trials, nil
			}
		}
		return nil, fmt.Errorf("case %q is in none of the selected suites", caseID)
	case TierFast, TierFull:
		if caseID != "" {
			return nil, errors.New("-case selects a smoke run; fast and full always run their whole workload")
		}
		if fr == nil || agentSuite == nil {
			return nil, fmt.Errorf("the %s tier runs both -suite %s and -agent-suite %s", tier, SuiteFR, SuiteAgentFailures)
		}
		w := Workloads[tier]
		for _, c := range fr.Cases {
			if tier == TierFull || c.Fast {
				add(fr, c, w.FRTrials)
			}
		}
		for _, c := range agentSuite.Cases {
			add(agentSuite, c, w.AgentTrials)
		}
		if len(trials) != w.Total() {
			return nil, fmt.Errorf("the %s tier planned %d trials, its fixed workload is %d", tier, len(trials), w.Total())
		}
		return trials, nil
	default:
		return nil, fmt.Errorf("unknown tier %q (smoke, fast, full)", tier)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalSHA256 hashes v's JSON encoding (map keys are sorted by
// encoding/json, so equal values hash equally).
func canonicalSHA256(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "unencodable"
	}
	return sha256Hex(raw)
}

// goldDate is a gold event date at its own precision.
type goldDate struct {
	Precision string
	Year      int
	Month     int
	Day       int
}

func parseGoldDate(s string) (goldDate, error) {
	var d goldDate
	switch len(s) {
	case 10:
		if _, err := fmt.Sscanf(s, "%4d-%2d-%2d", &d.Year, &d.Month, &d.Day); err != nil {
			return d, fmt.Errorf("date %q is not YYYY-MM-DD", s)
		}
		d.Precision = types.DatePrecisionDay
	case 7:
		if _, err := fmt.Sscanf(s, "%4d-%2d", &d.Year, &d.Month); err != nil {
			return d, fmt.Errorf("date %q is not YYYY-MM", s)
		}
		d.Precision = types.DatePrecisionMonth
	case 4:
		if _, err := fmt.Sscanf(s, "%4d", &d.Year); err != nil {
			return d, fmt.Errorf("date %q is not YYYY", s)
		}
		d.Precision = types.DatePrecisionYear
	default:
		return d, fmt.Errorf("date %q is not YYYY[-MM[-DD]]", s)
	}
	return d, nil
}
