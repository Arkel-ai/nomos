package types

import (
	"strings"
)

// Nomos state machines and enumerations.
//
// Every one of these is a plain Go string constant written into a plain
// varchar column. docs/PLAN.md §9.1(3) is explicit about why: a Postgres enum
// or a CHECK constraint turns every new workflow state into a schema change,
// and "AutoMigrate only" gives us no way to alter one. Validation is the
// application's job — see the Is*State helpers below.

// WorkflowDossierOrganizeV1 is the workflow_type of an ingestion run
// (services/ingest.go). Agent turns are WorkflowNomosAgent.
const WorkflowDossierOrganizeV1 = "dossier_organize_v1"

// Run states.
const (
	RunStatePending      = "pending"
	RunStateRunning      = "running"
	RunStateWaitingInput = "waiting_input"
	RunStateSucceeded    = "succeeded"
	RunStateFailed       = "failed"
	RunStateCancelled    = "cancelled"
)

// Step states. `queued` + `available_at` is the whole queue; `waiting_input`
// is the HITL park (§4.4), which dependents wait behind without a timer.
const (
	StepStateQueued       = "queued"
	StepStateRunning      = "running"
	StepStateWaitingInput = "waiting_input"
	StepStateSucceeded    = "succeeded"
	StepStateFailed       = "failed"
	StepStateCancelled    = "cancelled"
	StepStateSkipped      = "skipped"
)

// Step kinds. A kind maps a step onto exactly one runner pool (§4.2), which is
// why the set is closed and small: ocr and render are CPU-bound and run one at
// a time per pod, llm steps are slow but idle, db steps are cheap.
const (
	StepKindDB     = "db"
	StepKindLLM    = "llm"
	StepKindOCR    = "ocr"
	StepKindRender = "render"
)

// PoolForStepKind maps a step kind onto its runner pool name. Unknown kinds
// fall back to the db pool so a mis-tagged step can never occupy the single
// OCR or render slot.
func PoolForStepKind(kind string) string {
	switch kind {
	case StepKindOCR, StepKindRender, StepKindLLM:
		return kind
	default:
		return StepKindDB
	}
}

// Step attempt states.
const (
	AttemptStateRunning   = "running"
	AttemptStateSucceeded = "succeeded"
	AttemptStateFailed    = "failed"
	AttemptStateAbandoned = "abandoned" // lease expired, reaper requeued the step
)

// Run event types. `run_events` is the user-visible progress log and the
// cursor the thread replays from (§4.5b) — never security evidence, and never
// document content.
const (
	EventRunCreated      = "run.created"
	EventRunStateChanged = "run.state_changed"
	EventPlanReady       = "run.plan_ready"
	EventStepStarted     = "step.started"
	EventStepProgress    = "step.progress"
	EventStepSucceeded   = "step.succeeded"
	EventStepFailed      = "step.failed"
	EventStepRetrying    = "step.retrying"
	EventQuestionAsked   = "question.asked"
	EventQuestionAnswer  = "question.answered"
	EventArtifactReady   = "artifact.ready"
	EventMessageDelta    = "message.delta"    // conversational reply token
	EventMessageComplete = "message.complete" // conversational reply closed
)

// Run question states. A skip is recorded as an answer, never as silence
// (§4.4).
const (
	QuestionStatePending  = "pending"
	QuestionStateAnswered = "answered"
	QuestionStateSkipped  = "skipped"
)

// Question answer kinds.
const (
	QuestionKindSingle = "single"
	QuestionKindMulti  = "multi"
	QuestionKindText   = "text"
)

// Conversation states.
const (
	ConversationStateActive   = "active"
	ConversationStateArchived = "archived"
)

// Message roles.
const (
	MessageRoleUser      = "user"
	MessageRoleAssistant = "assistant"
	MessageRoleSystem    = "system"
)

// Message states. A conversational reply is persisted incrementally while it
// streams (§4.5b), so a message is durable before it is finished — `streaming`
// is the row a reconnecting client replays into.
//
// `reading` is the assistant turn of a message whose attachments the runner
// has not finished ingesting: it is answered once they are read, never
// against documents it cannot see yet.
const (
	MessageStateReading   = "reading"
	MessageStateStreaming = "streaming"
	MessageStateComplete  = "complete"
	MessageStateFailed    = "failed"
)

// Dossier states. Deletion is an explicit purge workflow, not a GORM soft
// delete — a soft delete purges nothing in S3, chunks or backups (§9.1(8)).
const (
	DossierStateActive       = "active"
	DossierStateArchived     = "archived"
	DossierStatePurgePending = "purge_pending"
)

// Piece classification states. Every proposed field is editable by the lawyer
// before export; low confidence is surfaced, never silently accepted (§7.1).
const (
	PieceStatePending   = "pending"   // stored, not yet extracted
	PieceStateExtracted = "extracted" // text extracted, not yet qualified
	PieceStateProposed  = "proposed"  // LLM qualification available for review
	PieceStateApproved  = "approved"  // the lawyer approved these fields
	PieceStateRejected  = "rejected"  // excluded from the bordereau
	PieceStateFailed    = "failed"    // extraction or qualification failed
)

// Date precision for a piece's document date — a dated act, a month, a year,
// or nothing legible.
const (
	DatePrecisionDay     = "day"
	DatePrecisionMonth   = "month"
	DatePrecisionYear    = "year"
	DatePrecisionUnknown = "unknown"
)

// Piece party roles.
const (
	PartyRoleEmitter     = "emetteur"
	PartyRoleRecipient   = "destinataire"
	PartyRoleParty       = "partie"
	PartyRoleThirdParty  = "tiers"
	PartyRoleRepresented = "represente"
)

// Asset kinds — what a stored S3 object is. Originals are never modified or
// renamed; renaming is metadata applied at export only (§7.1).
const (
	AssetKindOriginal  = "original"
	AssetKindPageImage = "page_image"
	AssetKindArtifact  = "artifact"
	AssetKindExport    = "export"
)

// Artifact kinds.
const (
	ArtifactKindBordereauDocx = "bordereau_docx"
	ArtifactKindExportZip     = "export_zip"
	ArtifactKindNoteDocx      = "note_docx"
)

// LLM call states and usage status. `unknown` usage is recorded and reconciled
// later; a character-count estimate is not auditable billing (§4.6).
const (
	LLMCallStateDispatched = "dispatched"
	LLMCallStateSucceeded  = "succeeded"
	LLMCallStateFailed     = "failed"

	UsageStatusKnown    = "known"
	UsageStatusUnknown  = "unknown"
	UsageStatusEstimate = "estimate"
)

// LLM call purposes — what the call was for, so cost can be attributed.
const (
	LLMPurposeQualify   = "qualify"
	LLMPurposeDraft     = "draft"
	LLMPurposeVisionOCR = "vision_ocr"
	LLMPurposeEmbed     = "embed"
	LLMPurposeRerank    = "rerank"
	LLMPurposeChat      = "chat"
	LLMPurposeWebSearch = "web_search"
)

// Citation source types. Exactly one of legal_version_id / piece_id is set;
// the pair is kept explicit rather than polymorphic (§9.1(7)).
//
// Agent citations and evidence spans use piece / legifrance / judilibre /
// web; `legal` stays readable on historical rows and is never written by the
// agent.
const (
	CitationSourceLegal      = "legal"
	CitationSourcePiece      = "piece"
	CitationSourceLegifrance = "legifrance"
	CitationSourceJudilibre  = "judilibre"
	CitationSourceWeb        = "web"
)

// Citation verification states. The three states are never collapsed into one
// green badge (§13): retrieved is "the machine found it", machine_checked is
// "the quote and the version still match the official source", and
// lawyer_verified is "a human took responsibility for it".
const (
	CitationRetrieved      = "retrieved"
	CitationMachineChecked = "machine_checked"
	CitationLawyerVerified = "lawyer_verified"
	CitationDrifted        = "drifted" // official source no longer matches
)

// Legal relation types. LLM-extracted relations are leads, never ground truth
// (§6.3) — hence the provenance/confidence/verification columns alongside.
const (
	RelationConfirms      = "confirms"
	RelationDistinguishes = "distinguishes"
	RelationOverrules     = "overrules"
	RelationRepeals       = "repeals"
	RelationAmends        = "amends"
	RelationCites         = "cites"
)

// Relation provenance.
const (
	ProvenanceSource = "source" // supplied by Légifrance / Judilibre metadata
	ProvenanceLLM    = "llm"    // extracted by a model — a lead only
	ProvenanceHuman  = "human"  // asserted by a lawyer
)

// Legal corpus sources.
const (
	SourceLegifrance = "legifrance"
	SourceJudilibre  = "judilibre"
	SourceDILA       = "dila"
	SourceLegiHF     = "legi_hf" // the pinned AgentPublic/legi bootstrap dataset
)

// Retrieval corpora, for POST /api/search.
const (
	CorpusPublic  = "public"  // legal_chunks — RRF(HNSW, french FTS) + rerank
	CorpusPrivate = "private" // piece_chunks — exact scan after the tenant filter
	CorpusBoth    = "both"
)

// legal_versions.state — the ONE canonical vocabulary, shared by the corpus
// loader that writes it and the retrieval filter that reads it.
//
// It is deliberately the Légifrance vocabulary rather than a generic
// active/inactive pair: these values are read by French lawyers, they come
// off the wire from the DILA sources in exactly this shape, and "vigueur" is
// the word the law itself uses. Retrieval defaults to LegalVersionInForce
// (docs/PLAN.md §6.4 — the applicable rule is the one in force), so a writer
// and a reader that disagree here silently return zero results.
const (
	LegalVersionInForce  = "vigueur" // en vigueur at the as-of date
	LegalVersionRepealed = "abroge"  // abrogé
	LegalVersionModified = "modifie" // modifié — superseded by a later version
	LegalVersionDeferred = "differe" // entrée en vigueur différée
	LegalVersionAnnulled = "annule"  // annulé
	LegalVersionUnknown  = "inconnu" // the source gave us nothing usable
)

// legalVersionStateAliases maps every spelling we have seen — from the DILA
// feeds, from the Hugging Face bootstrap corpus, and from this codebase's own
// earlier "active" default — onto the canonical values above.
var legalVersionStateAliases = map[string]string{
	"vigueur":      LegalVersionInForce,
	"en_vigueur":   LegalVersionInForce,
	"en vigueur":   LegalVersionInForce,
	"active":       LegalVersionInForce,
	"in_force":     LegalVersionInForce,
	"abroge":       LegalVersionRepealed,
	"abrogé":       LegalVersionRepealed,
	"abrogated":    LegalVersionRepealed,
	"repealed":     LegalVersionRepealed,
	"modifie":      LegalVersionModified,
	"modifié":      LegalVersionModified,
	"modified":     LegalVersionModified,
	"differe":      LegalVersionDeferred,
	"différé":      LegalVersionDeferred,
	"deferred":     LegalVersionDeferred,
	"annule":       LegalVersionAnnulled,
	"annulé":       LegalVersionAnnulled,
	"annulled":     LegalVersionAnnulled,
	"vigueur_diff": LegalVersionDeferred,
}

// NormalizeLegalVersionState folds a source-supplied state onto the canonical
// vocabulary. Every writer of legal_versions.state MUST call it, so the value
// retrieval filters on can never drift from the value the loader stored.
// An unrecognised state is preserved lowercased rather than discarded — losing
// it would silently promote an abrogated article into the in-force set.
func NormalizeLegalVersionState(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return LegalVersionUnknown
	}
	if canonical, ok := legalVersionStateAliases[s]; ok {
		return canonical
	}
	return s
}

// Skill and memory states (§8). Skill definitions are immutable versions; a
// mutable blob makes an earlier pleading unreproducible.
const (
	SkillStateDraft    = "draft"
	SkillStateActive   = "active"
	SkillStateArchived = "archived"

	MemoryStateProposed = "proposed"
	MemoryStateApproved = "approved"
	MemoryStateRevoked  = "revoked"

	// MemoryProvenanceApproved is memory the user explicitly asked for;
	// MemoryProvenanceInferred is a suggestion the model made. Nomos never
	// promotes inferred to approved on its own (§8).
	MemoryProvenanceApproved = "user_approved"
	MemoryProvenanceInferred = "inferred"
)

// Memory scopes.
const (
	MemoryScopeUser = "user"
	MemoryScopeFirm = "firm"
)

// Corpus sync states.
const (
	CorpusSyncIdle    = "idle"
	CorpusSyncRunning = "running"
	CorpusSyncFailed  = "failed"
)

// Casbin objects used by the Nomos routes. The Casbin domain is the
// tenant_id, never the template's "*" — that would erase tenancy (§6.1).
const (
	ObjectDossier = "dossier"
	ObjectAdmin   = "admin"

	ActionRead  = "read"
	ActionWrite = "write"
)

var (
	runStates = map[string]bool{
		RunStatePending: true, RunStateRunning: true, RunStateWaitingInput: true,
		RunStateSucceeded: true, RunStateFailed: true, RunStateCancelled: true,
	}
	stepStates = map[string]bool{
		StepStateQueued: true, StepStateRunning: true, StepStateWaitingInput: true,
		StepStateSucceeded: true, StepStateFailed: true, StepStateCancelled: true,
		StepStateSkipped: true,
	}
	pieceStates = map[string]bool{
		PieceStatePending: true, PieceStateExtracted: true, PieceStateProposed: true,
		PieceStateApproved: true, PieceStateRejected: true, PieceStateFailed: true,
	}
	citationStates = map[string]bool{
		CitationRetrieved: true, CitationMachineChecked: true,
		CitationLawyerVerified: true, CitationDrifted: true,
	}
)

// IsRunState reports whether s is a known run state.
func IsRunState(s string) bool { return runStates[s] }

// IsStepState reports whether s is a known step state.
func IsStepState(s string) bool { return stepStates[s] }

// IsPieceState reports whether s is a known piece classification state.
func IsPieceState(s string) bool { return pieceStates[s] }

// IsCitationState reports whether s is a known citation verification state.
func IsCitationState(s string) bool { return citationStates[s] }

// TerminalRunState reports whether a run in state s will never change again.
func TerminalRunState(s string) bool {
	return s == RunStateSucceeded || s == RunStateFailed || s == RunStateCancelled
}

// TerminalStepState reports whether a step in state s will never change again.
func TerminalStepState(s string) bool {
	return s == StepStateSucceeded || s == StepStateFailed ||
		s == StepStateCancelled || s == StepStateSkipped
}

// ---------------------------------------------------------------------------
// Web research — the naming contract between the planner and the loop
// ---------------------------------------------------------------------------

// The web research loop derives a step key from the QUERY ITSELF, so the same
// question always produces the same key and (run_id, step_key) uniqueness does
// the loop's deduplication for free — a later round that re-proposes an
// earlier query collides and is pruned instead of being searched twice.
//
// That only holds if the seed round and the loop agree on the derivation, and
// they live either side of a package boundary the workflows package keeps on
// purpose (it must stay plannable without importing poppler, S3 or the
// corpus). So the derivation lives here, with the other shared naming
// contracts, and neither side owns a private copy.

// MaxWebQueryRunes bounds a web research query. It is a CONFIDENTIALITY bound
// before it is a cost one: a query long enough to carry dossier prose must not
// reach a search backend that sits outside the provider allowlist. The hard
// gate is applied in services, on every query, whatever wrote it.
const MaxWebQueryRunes = 200
