package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Agent runtime contracts, under the owner decisions listed in docs/PLAN.md.
// Track 0 owns this file: a change here is a wire change, so the
// TypeScript mirror in front/src/lib/api/agent-contracts.ts moves with it.

// ---------------------------------------------------------------------------
// Identity and hard limits
// ---------------------------------------------------------------------------

// WorkflowNomosAgent is persisted on runs.workflow_type for agent turns
// (§3.1). It names the runtime protocol, not a business workflow.
const (
	WorkflowNomosAgent        = "nomos_agent"
	WorkflowNomosAgentVersion = 1
)

// MaxAgentDispatches is the hard ceiling on controller-model dispatches per
// agent turn, retries and corrective generations included (§3.4). Each is
// reserved in Postgres before the provider request. Configuration may lower
// it for tests, never raise it; answering a clarification never resets it.
const MaxAgentDispatches = 40

// Ceilings shared by the runtime, the tools and the gate.
const (
	MaxAgentInstructionChars  = 20000 // instruction cap at intake
	MaxAgentParallelReads     = 4     // read-only calls accepted in one decision (§3.3)
	MaxAgentReviewCalls       = 8     // review_output calls per run (§2)
	MaxAgentWebSearches       = 10    // search_web requests per run (§2)
	MaxAgentQuestionsPerSet   = 8     // questions in one ask_lawyer set (§5.4)
	MaxAgentRepeatedFailures  = 3     // identical failing calls before no_progress (§3.7)
	MaxAgentSourceOutages     = 5     // final-gate refusals on an unreachable official source before the turn ends
	DefaultAgentActiveSeconds = 1800  // active-work deadline, parked time excluded (§3.4)
)

// ProgressNoteArg is the argument every tool schema requires: one public
// intent line, shown in the thread exactly as written (owner decision 1). It
// is never evidence and never chain-of-thought.
const (
	ProgressNoteArg      = "progress_note"
	MaxProgressNoteRunes = 140
)

// EventAgentActivity is the durable thread event behind one activity row.
// The thread rebuilds the agent's activity from these on reload.
const EventAgentActivity = "agent.activity"

// AgentCallID is the server's identity for the ordinal-th call (0-based) of a
// controller dispatch. Provider function-call ids are only unique within one
// response, so every durable row and event keys on this instead.
func AgentCallID(dispatch, ordinal int) string {
	return fmt.Sprintf("d%d.c%d", dispatch, ordinal)
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// Tool names — the Slice 1 catalogue (§6.1). There is no run_workflow, shell,
// HTTP, code-execution or subagent tool.
const (
	ToolListPieces      = "list_pieces"
	ToolSearchPieces    = "search_pieces"
	ToolReadPiece       = "read_piece"
	ToolSearchLaw       = "search_law"
	ToolReadLaw         = "read_law"
	ToolSearchCaselaw   = "search_caselaw"
	ToolReadDecision    = "read_decision"
	ToolSearchWeb       = "search_web"
	ToolReadEvidence    = "read_evidence"
	ToolListCompetences = "list_competences"
	ToolLoadCompetence  = "load_competence"
	ToolAskLawyer       = "ask_lawyer"
	ToolWriteDocument   = "write_document"
	ToolEditDocument    = "edit_document"
	ToolReadDocument    = "read_document"
	ToolCalculate       = "calculate"
	ToolVerifyOutput    = "verify_output"
	ToolReviewOutput    = "review_output"
	ToolRenderDocument  = "render_document"
	ToolExportDossier   = "export_dossier"
	ToolSubmitResult    = "submit_result"
)

// Tool classes: which agent step kind executes a call (§3.3). The server
// derives step kinds, keys, dependencies and pools; the model never does.
const (
	ToolClassDB       = "db"       // agent_tool_db: private reads, deterministic work, writes
	ToolClassExternal = "external" // agent_tool_external: public retrieval, bounded review
	ToolClassRender   = "render"   // agent_tool_render: pandoc into a private staged object
)

// AgentToolDefinition is one native function offered to the controller.
// Parameters is a strict JSON Schema object (additionalProperties false)
// whose required list contains ProgressNoteArg; services.ValidateToolDefinition
// enforces both.
type AgentToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`

	// Server-side scheduling, never sent to the model.
	Class string `json:"-"` // ToolClass*
	// ReadOnly calls may share a decision, up to MaxAgentParallelReads.
	// Writes execute serially.
	ReadOnly bool `json:"-"`
	// Exclusive calls (ask_lawyer, submit_result) must be a decision's only
	// call; a mixed batch is answered with ToolCodeExclusiveCall.
	Exclusive bool `json:"-"`
}

// ProgressNoteSchema is the JSON Schema property every tool definition
// carries under ProgressNoteArg. maxLength counts code points, as runes do.
func ProgressNoteSchema() map[string]any {
	return map[string]any{
		"type":      "string",
		"minLength": 1,
		"maxLength": MaxProgressNoteRunes,
		"description": "Intention publique de cet appel, en une phrase de 140 caractères au plus, " +
			"affichée telle quelle à l'avocat. Ni raisonnement interne, ni extrait de pièce.",
	}
}

// ToolArgs is embedded in every tool's argument struct so the progress note
// decodes the same way everywhere.
type ToolArgs struct {
	ProgressNote string `json:"progress_note"`
}

// AgentToolCall is one native function call from a controller response.
type AgentToolCall struct {
	ID         string          `json:"id"`          // AgentCallID(dispatch, ordinal)
	ProviderID string          `json:"provider_id"` // pairs the tool message on the next dispatch
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

// AgentDecision is one controller dispatch as recorded by agent_decide.
// Content is assistant text returned alongside calls: private, never
// delivered — a final answer only reaches the lawyer through submit_result.
type AgentDecision struct {
	Dispatch     int             `json:"dispatch"`
	LLMCallID    string          `json:"llm_call_id"`
	Model        string          `json:"model"` // as returned by the provider
	FinishReason string          `json:"finish_reason"`
	Calls        []AgentToolCall `json:"calls"`
	Content      string          `json:"content,omitempty"`
	// Rejection is empty for an accepted decision; otherwise an AgentOutcome
	// correction code, returned to the next dispatch as an observation.
	Rejection string `json:"rejection,omitempty"`
}

// Observation statuses. `empty` is a successful zero-result call — never an
// error, and never "nothing applicable exists".
const (
	ObservationOK    = "ok"
	ObservationEmpty = "empty"
	ObservationError = "error"
)

// Observation error codes (§3.7). Expected failures become observations the
// agent can recover from; integrity failures fail the run instead.
const (
	ToolCodeInvalidArguments    = "invalid_arguments"
	ToolCodeUnknownTool         = "unknown_tool"
	ToolCodeUnknownReference    = "unknown_reference" // unknown OR unauthorized: non-disclosing
	ToolCodeMissingDossier      = "missing_dossier"
	ToolCodeToolUnavailable     = "tool_unavailable" // a prerequisite is not configured
	ToolCodeSourceUnavailable   = "source_unavailable"
	ToolCodeConnectorFailure    = "connector_failure"   // transient, retries exhausted
	ToolCodeUnsafePublicQuery   = "unsafe_public_query" // owner decision 2; data names the category
	ToolCodeProcessingLimit     = "processing_limit"    // context or processing limit
	ToolCodeRevisionConflict    = "revision_conflict"   // stale expected hash
	ToolCodeVerificationRefused = "verification_refused"
	ToolCodeBudgetExhausted     = "budget_exhausted" // review or web budget
	ToolCodeExclusiveCall       = "exclusive_call"   // ask_lawyer/submit_result batched with another call
	ToolCodeTooManyCalls        = "too_many_calls"
	ToolCodePublicationRefused  = "publication_refused" // the verified result could not be published as submitted
	ToolCodeScopeChanged        = "scope_changed"       // the dossier moved between the final gate and publication
)

// ToolObservation is the common envelope of every tool result (§3.7).
type ToolObservation struct {
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Status    string          `json:"status"` // Observation*
	Code      string          `json:"code,omitempty"`
	Retryable bool            `json:"retryable"`
	Data      json.RawMessage `json:"data,omitempty"`
	// EvidenceIDs are agent_evidence_spans made available by this call.
	EvidenceIDs []string `json:"evidence_ids"`
	// ReadReceiptIDs are agent_read_receipts proposed by this call. They count
	// toward coverage only once the next accepted dispatch consumed the text.
	ReadReceiptIDs []string `json:"read_receipt_ids"`
	NextCursor     string   `json:"next_cursor,omitempty"`
	// Truncated says text was omitted; NextCursor reaches it. Omitted text is
	// never counted as read.
	Truncated bool `json:"truncated"`
}

// ---------------------------------------------------------------------------
// Activity rows (owner decision 1)
// ---------------------------------------------------------------------------

// Activity statuses: `running` when a call starts, then one of the
// observation statuses. Rows with the same call_id replace each other.
const (
	ActivityRunning = "running"
	ActivityOK      = ObservationOK
	ActivityEmpty   = ObservationEmpty
	ActivityError   = ObservationError
)

// ActivityCodeInterrupted closes a row whose call never finished: the run
// ended (cancelled, failed, out of budget) while it ran or waited.
const ActivityCodeInterrupted = "interrupted"

// Activity object kinds — what a call touched. The tool name says what was
// done to it.
const (
	ActivityObjectPieces       = "pieces"       // the dossier inventory
	ActivityObjectPiece        = "piece"        // pièce n° + title, detail = range read
	ActivityObjectQuery        = "query"        // label = the search text (public for law/caselaw/web)
	ActivityObjectLaw          = "law"          // an article version
	ActivityObjectDecision     = "decision"     // a court decision
	ActivityObjectEvidence     = "evidence"     // re-read frozen evidence
	ActivityObjectCompetence   = "competence"   // a compétence listed or loaded
	ActivityObjectQuestion     = "question"     // a question asked to the lawyer
	ActivityObjectDocument     = "document"     // a revision created, edited or read
	ActivityObjectVerification = "verification" // verify_output counts
	ActivityObjectReview       = "review"       // review_output
	ActivityObjectCalculation  = "calculation"
	ActivityObjectArtifact     = "artifact" // a rendered document or export
	ActivityObjectSubmission   = "submission"
)

// AgentActivityObject is the concrete object of a call. Never raw JSON,
// arguments or hidden reasoning.
type AgentActivityObject struct {
	Kind   string `json:"kind"` // ActivityObject*
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// AgentActivityCounts are the numbers an activity row may show. Pointers so
// a real zero ("0 résultat") survives serialisation.
type AgentActivityCounts struct {
	Results   *int `json:"results,omitempty"`   // hits returned
	Total     *int `json:"total,omitempty"`     // hits or pieces available
	Segments  *int `json:"segments,omitempty"`  // segments read
	Warnings  *int `json:"warnings,omitempty"`  // verification warnings
	Blocking  *int `json:"blocking,omitempty"`  // verification blockers
	Questions *int `json:"questions,omitempty"` // questions asked
}

// AgentActivityPayload is the payload of an EventAgentActivity event.
type AgentActivityPayload struct {
	Dispatch     int                  `json:"dispatch"`
	CallID       string               `json:"call_id"`
	Tool         string               `json:"tool"`
	ProgressNote string               `json:"progress_note"`
	Status       string               `json:"status"`         // Activity*
	Code         string               `json:"code,omitempty"` // why an error row ended, e.g. ActivityCodeInterrupted
	Object       AgentActivityObject  `json:"object"`
	Counts       *AgentActivityCounts `json:"counts,omitempty"`
	At           int64                `json:"at"` // unix seconds
}

// ---------------------------------------------------------------------------
// Intake and status
// ---------------------------------------------------------------------------

// AgentTurnRequest is the input of services.StartAgentTurn. Tenant and actor
// are resolved by the caller from the session, never from a request body.
// (types.AgentRequest is the reference /agent route's body, kept unchanged.)
type AgentTurnRequest struct {
	TenantID       string   `json:"tenant_id"`
	ActorID        string   `json:"actor_id"`
	ConversationID *string  `json:"conversation_id"`
	DossierID      *string  `json:"dossier_id"`
	Instruction    string   `json:"instruction"`
	AttachmentIDs  []string `json:"attachment_ids,omitempty"`
	// SkillSlug is the compétence picked in the `/` menu (owner decision 5).
	// It is pinned at intake; an invalid pick is an error, never free text.
	SkillSlug string `json:"skill_slug,omitempty"`
	// RequestID is the client's retry key; empty means the server mints one.
	RequestID string `json:"request_id,omitempty"`
	// Entry names the endpoint the request came through (AgentEntry* in
	// services). It is part of the canonical request a retry must repeat.
	Entry string `json:"entry,omitempty"`
	// OpenConversation makes the turn create its thread in the same
	// transaction (POST /conversations with an inline first message), titled
	// ConversationTitle and bound to DossierID: a retried request can then
	// never leave a second thread behind. ConversationID must be nil.
	OpenConversation  bool   `json:"open_conversation,omitempty"`
	ConversationTitle string `json:"conversation_title,omitempty"`
	// BindDossier binds the existing thread to DossierID in the turn's
	// transaction (a message sent from the composer with a new dossier).
	BindDossier bool `json:"bind_dossier,omitempty"`
}

// AgentTurnStatus is the agent part of a turn on the wire.
type AgentTurnStatus struct {
	RunID         string `json:"run_id"`
	Dispatches    int    `json:"dispatches"`
	MaxDispatches int    `json:"max_dispatches"`
	Outcome       string `json:"outcome,omitempty"` // AgentOutcome*, empty while running
	DeadlineAt    int64  `json:"deadline_at"`
	Parked        bool   `json:"parked"` // waiting on the lawyer
}

// AgentStartResult is what StartAgentTurn committed. The caller maps it onto
// MessageCreateResponse.
type AgentStartResult struct {
	Turn             AgentTurnStatus
	UserMessage      Message
	AssistantMessage Message
	Cursor           uint64
	// Replayed is true when the request id and body matched an already
	// committed turn: nothing new was inserted.
	Replayed bool
	// RequestID is the turn's retry key: the client's, or the one minted.
	RequestID string
}

// Agent outcomes, stored on agent_turns.outcome. The correction codes are
// also returned to the controller when a dispatch is rejected; they become
// the turn's outcome only when they end it.
const (
	AgentOutcomeCompleted  = "completed"            // gate passed, result published
	AgentOutcomePartial    = "partial"              // published, explicitly incomplete
	AgentOutcomeRefused    = "verification_refused" // the gate refused the submission
	AgentOutcomeTurnLimit  = "agent_turn_limit"
	AgentOutcomeDeadline   = "agent_deadline"
	AgentOutcomeNoProgress = "no_progress"
	AgentOutcomeCancelled  = "cancelled"
	AgentOutcomeFailed     = "agent_failed" // integrity or accounting failure

	AgentOutcomeSubmissionRequired = "submission_required" // free prose instead of submit_result
	AgentOutcomeInvalidToolCall    = "invalid_tool_call"
	AgentOutcomeModelRefusal       = "model_refusal"
	AgentOutcomeModelTruncated     = "model_truncated"
)

// ---------------------------------------------------------------------------
// Task contract (§4.2)
// ---------------------------------------------------------------------------

// Requested outputs. Every kind but dossier_export is also a document
// revision kind; `answer` is the structured reply submitted with submit_result.
const (
	AgentOutputAnswer        = "answer"
	AgentOutputChronology    = "chronology"
	AgentOutputSynthesis     = "synthesis"
	AgentOutputLegalNote     = "legal_note"
	AgentOutputActe          = "acte"
	AgentOutputBordereau     = "bordereau"
	AgentOutputDossierExport = "dossier_export"
)

// Source scope modes. An explicit empty subset is not "all pieces".
const (
	ScopeAllPieces = "all_pieces"
	ScopeExplicit  = "explicit" // Included is the whole scope, possibly empty
	ScopeNone      = "none"     // no dossier: private tools answer missing_dossier
)

// Contract provenance: where a scope or contract change came from. The agent
// cannot narrow scope, drop exhaustiveness, change the deliverable or
// manufacture a material date without one of these.
const (
	ContractFromUserMessage  = "user_message"
	ContractFromLawyerAnswer = "lawyer_answer"
)

// ContractProvenance points at the message or answer behind a change.
type ContractProvenance struct {
	Source      string `json:"source"` // ContractFrom*
	MessageID   string `json:"message_id,omitempty"`
	QuestionKey string `json:"question_key,omitempty"`
}

// ScopeEntry is one piece explicitly included or excluded.
type ScopeEntry struct {
	PieceID    string             `json:"piece_id"`
	Provenance ContractProvenance `json:"provenance"`
}

// AgentSourceScope is the documentary scope frozen for the turn. A ranked
// search never redefines it.
type AgentSourceScope struct {
	Mode      string       `json:"mode"` // Scope*
	DossierID string       `json:"dossier_id,omitempty"`
	Included  []ScopeEntry `json:"included,omitempty"` // ScopeExplicit only
	Excluded  []ScopeEntry `json:"excluded,omitempty"` // ScopeAllPieces only
}

// PinnedCompetence is the compétence version resolved at intake.
type PinnedCompetence struct {
	SkillID        string `json:"skill_id"`
	SkillVersionID string `json:"skill_version_id"`
	Slug           string `json:"slug"`
	Version        int    `json:"version"`
}

// Required-decision states.
const (
	DecisionOpen     = "open"
	DecisionAsked    = "asked"
	DecisionAnswered = "answered"
	DecisionSkipped  = "skipped"
)

// RequiredDecision is a material choice only the lawyer can make.
type RequiredDecision struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	State       string `json:"state"`                  // Decision*
	QuestionKey string `json:"question_key,omitempty"` // once asked
}

// DateInterval bounds a period. Each bound is inclusive at its own
// precision; a single date has From == To. Description keeps a boundary the
// lawyer stated as an event ("du contrat de travail au solde de tout
// compte") — it is never converted into a manufactured date.
type DateInterval struct {
	From        *PartialDate `json:"from,omitempty"`
	To          *PartialDate `json:"to,omitempty"`
	Description string       `json:"description,omitempty"`
}

// AgentTaskContractVersion pins the contract's JSON shape.
const AgentTaskContractVersion = 1

// AgentTaskContract is what the turn must deliver, stored on agent_turns.
type AgentTaskContract struct {
	Version     int              `json:"version"`
	Revision    int              `json:"revision"` // bumped by every lawyer-backed change
	Instruction string           `json:"instruction"`
	OutputKinds []string         `json:"output_kinds"` // AgentOutput*
	Scope       AgentSourceScope `json:"scope"`
	Exhaustive  bool             `json:"exhaustive"`
	// EventPeriod limits which events are selected, never which documents
	// may be read: a later attestation stays evidence about earlier events.
	EventPeriod *DateInterval `json:"event_period,omitempty"`
	// StatuteApplicability and CaseLawCutoff are independent dates.
	StatuteApplicability *DateInterval      `json:"statute_applicability,omitempty"`
	CaseLawCutoff        *PartialDate       `json:"caselaw_cutoff,omitempty"`
	Competence           *PinnedCompetence  `json:"competence,omitempty"`
	RequiredDecisions    []RequiredDecision `json:"required_decisions,omitempty"`
}

// ---------------------------------------------------------------------------
// Dates and money (§4.2)
// ---------------------------------------------------------------------------

// PartialDate is a date at the precision its source gives (DatePrecision*).
// Month and Day are zero when unknown — never a manufactured first of the
// month or of the year.
type PartialDate struct {
	Precision string `json:"precision"`
	Year      int    `json:"year,omitempty"`
	Month     int    `json:"month,omitempty"`
	Day       int    `json:"day,omitempty"`
}

// Date roles: when something happened, when a document was dated, when it
// was received. A document date is never substituted for an event date.
const (
	DateRoleEvent    = "event"
	DateRoleDocument = "document"
	DateRoleReceipt  = "receipt"
)

// ClaimDate is a dated assertion inside a claim.
type ClaimDate struct {
	PartialDate
	Role string `json:"role"` // DateRole*
}

// Money qualifiers. Empty when the source states none.
const (
	MoneyGross = "gross"
	MoneyNet   = "net"
	MoneyHT    = "ht"
	MoneyTTC   = "ttc"
)

// Money is an amount in integer minor units — never float64.
type Money struct {
	MinorUnits int64  `json:"minor_units"`
	Currency   string `json:"currency"` // ISO 4217, e.g. "EUR"
	Qualifier  string `json:"qualifier,omitempty"`
}

// ---------------------------------------------------------------------------
// Document revisions (§4.2)
// ---------------------------------------------------------------------------

// Block roles.
const (
	BlockHeading       = "heading"
	BlockParagraph     = "paragraph"
	BlockListItem      = "list_item"
	BlockChronologyRow = "chronology_row"
	BlockBordereauRow  = "bordereau_row"
	BlockLimitation    = "limitation"
	BlockWarning       = "warning"
)

// Claim kinds.
const (
	ClaimFactual        = "factual"
	ClaimLegal          = "legal"
	ClaimAnalysis       = "analysis"
	ClaimLawyerProvided = "lawyer_provided"
	ClaimDerived        = "derived_calculation"
)

// ClaimQuote is an exact quotation and the evidence span it comes from.
type ClaimQuote struct {
	EvidenceID string `json:"evidence_id"`
	Text       string `json:"text"`
}

// DocumentClaim is one checkable assertion inside a block. Start/End are
// half-open Unicode code-point offsets into the block's Text.
type DocumentClaim struct {
	ID          string   `json:"id"`
	Start       int      `json:"start"`
	End         int      `json:"end"`
	Kind        string   `json:"kind"` // Claim*
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	// AnswerKeys are the lawyer answers (question keys) a claim rests on.
	AnswerKeys []string `json:"answer_keys,omitempty"`
	// CalculationCallID is the calculate call behind a derived amount.
	CalculationCallID string       `json:"calculation_call_id,omitempty"`
	Dates             []ClaimDate  `json:"dates,omitempty"`
	Amounts           []Money      `json:"amounts,omitempty"`
	Quotes            []ClaimQuote `json:"quotes,omitempty"`
}

// ChronologyRow is the structured part of a chronology_row block: the block
// Text is the event, its claims carry the sources.
type ChronologyRow struct {
	Date ClaimDate `json:"date"`
}

// BordereauRow is the structured part of a bordereau_row block: the block
// Text is the piece's name as listed.
type BordereauRow struct {
	Ordinal int          `json:"ordinal"`
	PieceID string       `json:"piece_id"`
	Date    *PartialDate `json:"date,omitempty"`
}

// DocumentBlock is one block with a stable id: edits address blocks by id.
type DocumentBlock struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`            // Block*
	Level      int             `json:"level,omitempty"` // heading or list depth
	Text       string          `json:"text"`
	Claims     []DocumentClaim `json:"claims,omitempty"`
	Chronology *ChronologyRow  `json:"chronology,omitempty"`
	Bordereau  *BordereauRow   `json:"bordereau,omitempty"`
}

// DocumentSchemaVersion pins DocumentRevisionBody's shape.
const DocumentSchemaVersion = 1

// DocumentRevisionBody is the typed body stored on agent_document_revisions.
type DocumentRevisionBody struct {
	SchemaVersion int             `json:"schema_version"`
	Blocks        []DocumentBlock `json:"blocks"`
}

// DocumentContentSHA256 is the content hash stored on a revision and bound
// by its verification. Writers and verifiers must both use it.
func DocumentContentSHA256(kind, title string, body DocumentRevisionBody) (string, error) {
	raw, err := json.Marshal(struct {
		Kind  string               `json:"kind"`
		Title string               `json:"title"`
		Body  DocumentRevisionBody `json:"body"`
	}{kind, title, body})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
