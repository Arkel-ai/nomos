package types

import (
	"time"

	"github.com/pgvector/pgvector-go"
)

// Nomos data model — docs/PLAN.md §9.
//
// Conventions, all of them load-bearing:
//
//   - UUID strings are the primary key of anything externally visible; uint64
//     is reserved for high-volume internal rows (run_events, the two chunk
//     tables, audit_events).
//   - Timestamps are int64 unix seconds with autoCreateTime/autoUpdateTime,
//     the project convention. The one exception is the lease/queue clock on run_steps:
//     those are timestamptz because claiming compares against the *Postgres*
//     clock, never a pod clock (§9). Mixing the two here would be a bug, not
//     an inconsistency.
//   - Every index has an EXPLICIT, STABLE name. AutoMigrate never drops an
//     index, so a renamed index means two indexes forever — one of which may
//     be multiple gigabytes (§9.1(9)).
//   - tenant_id is on every private row. It must also be in the S3 key, in
//     the Casbin domain and in the repository signature (§9.1(12)).
//   - No foreign key or association against user/session/account/verification.
//     Better Auth owns those tables; a user id is an indexed string column.
//   - No GORM soft deletes. Retention and purge are an explicit workflow —
//     a soft delete purges nothing in S3, vectors, or backups (§9.1(8)).

// ---------------------------------------------------------------------------
// Slice 1 — tenancy, dossiers, pieces
// ---------------------------------------------------------------------------

// Firm is the tenant. Its id is the Casbin domain and the tenant_id carried by
// every private row — the starter's `"*"` domain would erase tenancy (§6.1).
type Firm struct {
	ID        string `gorm:"primaryKey;size:64" json:"id"`
	Name      string `gorm:"type:text;not null" json:"name"`
	CreatedBy string `gorm:"size:64;index:idx_firms_created_by" json:"created_by"`
	CreatedAt int64  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64  `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Firm) TableName() string { return "firms" }

// Asset is one immutable object in the app's private S3 prefix: an uploaded
// original, a rendered page image, a generated artifact, an export bundle.
// Originals are never modified or renamed — renaming is metadata applied at
// export time only (§7.1). sha256 is computed before parsing.
type Asset struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_assets_tenant_created,priority:1" json:"tenant_id"`
	S3Key    string `gorm:"size:1024;not null;uniqueIndex:idx_assets_s3_key" json:"s3_key"`
	Kind     string `gorm:"size:32;not null" json:"kind"` // AssetKind*
	Mime     string `gorm:"size:255" json:"mime"`         // sniffed, never trusted from the extension
	Size     int64  `json:"size"`
	SHA256   string `gorm:"size:64;index:idx_assets_sha256" json:"sha256"`

	CreatedBy string `gorm:"size:64" json:"created_by"`
	CreatedAt int64  `gorm:"autoCreateTime;index:idx_assets_tenant_created,priority:2,sort:desc" json:"created_at"`
}

func (Asset) TableName() string { return "assets" }

// Dossier is a matter: the collection of pieces a conversation can bind to.
type Dossier struct {
	ID              string `gorm:"primaryKey;size:64" json:"id"`
	TenantID        string `gorm:"size:64;not null;index:idx_dossiers_tenant_updated,priority:1" json:"tenant_id"`
	Title           string `gorm:"type:text;not null" json:"title"`
	ClientReference string `gorm:"size:255" json:"client_reference"`
	MatterType      string `gorm:"size:64" json:"matter_type"`
	State           string `gorm:"size:32;not null;default:active" json:"state"` // DossierState*
	// TitlePending marks a placeholder title: the dossier was opened from the
	// composer around dropped files, and the first turn of its thread names
	// it. Cleared the moment a real title is written.
	TitlePending bool `gorm:"not null;default:false" json:"title_pending"`

	CreatedBy string `gorm:"size:64;index:idx_dossiers_created_by" json:"created_by"`
	// PurgeAfter is the explicit retention deadline (unix seconds, 0 = none).
	// The purge workflow reads it; nothing deletes implicitly.
	PurgeAfter int64 `json:"purge_after"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index:idx_dossiers_tenant_updated,priority:2,sort:desc" json:"updated_at"`
}

func (Dossier) TableName() string { return "dossiers" }

// Piece is one document in a dossier, with the fields the workflow proposes
// and the lawyer approves. The extraction JSON keeps the model's raw output;
// the typed columns are what the bordereau is built from, because they are
// what gets filtered, sorted and renumbered.
type Piece struct {
	ID        string `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string `gorm:"size:64;not null;index:idx_pieces_tenant_dossier_ordinal,priority:1" json:"tenant_id"`
	DossierID string `gorm:"size:64;not null;index:idx_pieces_tenant_dossier_ordinal,priority:2;index:idx_pieces_dossier_docdate,priority:1" json:"dossier_id"`
	// SourceAssetID is unique: one stored original becomes at most one piece,
	// which is what makes re-ingestion idempotent.
	SourceAssetID string `gorm:"size:64;not null;uniqueIndex:idx_pieces_source_asset" json:"source_asset_id"`

	OriginalName string `gorm:"type:text;not null" json:"original_name"`
	ProposedName string `gorm:"type:text" json:"proposed_name"`
	FinalName    string `gorm:"type:text" json:"final_name"`
	Ordinal      int    `gorm:"not null;default:0;index:idx_pieces_tenant_dossier_ordinal,priority:3" json:"ordinal"`

	// DocumentDate is the date of the act (unix seconds), nullable because a
	// document may carry no legible date at all. DatePrecision says how much
	// of it to trust — never render a fabricated day.
	DocumentDate  *int64 `gorm:"index:idx_pieces_dossier_docdate,priority:2" json:"document_date"`
	DatePrecision string `gorm:"size:16" json:"date_precision"` // DatePrecision*

	PieceKind string `gorm:"size:64" json:"piece_kind"`
	Title     string `gorm:"type:text" json:"title"`
	// Summary is the qualification's short factual account of the piece. It
	// is what the planner reads in place of the document. Empty for a piece
	// qualified before summaries existed.
	Summary string `gorm:"type:text" json:"summary"`

	ClassificationState string  `gorm:"size:32;not null;default:pending;index:idx_pieces_state" json:"classification_state"`
	Confidence          float64 `json:"confidence"`

	// ExtractionVersion pins the parser + prompt + schema revision that
	// produced Extraction, so a re-run is comparable and a regression is
	// attributable.
	ExtractionVersion string `gorm:"size:64" json:"extraction_version"`
	Extraction        JSONB  `gorm:"type:jsonb" json:"extraction,omitempty"`

	// PageCount and TextChars are shapes, not content — safe to log.
	PageCount int `json:"page_count"`
	TextChars int `json:"text_chars"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64 `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Piece) TableName() string { return "pieces" }

// PiecePartie is a party named by a piece, promoted out of the extraction JSON
// so name reconciliation and timeline queries stay cheap (§9.1(6)).
type PiecePartie struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_piece_parties_tenant_piece_role,priority:1" json:"tenant_id"`
	PieceID  string `gorm:"size:64;not null;index:idx_piece_parties_tenant_piece_role,priority:2" json:"piece_id"`
	Role     string `gorm:"size:32;not null;index:idx_piece_parties_tenant_piece_role,priority:3" json:"role"` // PartyRole*

	DisplayName string `gorm:"type:text;not null" json:"display_name"`
	// NormalizedName is the folded/unaccented form used to reconcile the same
	// party across pieces. Indexed on its own for exactly that lookup.
	NormalizedName string `gorm:"size:512;index:idx_piece_parties_normalized" json:"normalized_name"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (PiecePartie) TableName() string { return "piece_parties" }

// ---------------------------------------------------------------------------
// The conversational surface — the product's primary entry point (§1, §4.5b,
// §11). A thread carries plain conversational turns and long-running missions,
// interleaved. There is no separate "run page".
// ---------------------------------------------------------------------------

// Conversation is a thread. DossierID is nullable on purpose: a conversation
// exists before it is bound to a collection, and the collection pill (§11) is
// how the lawyer binds it later.
type Conversation struct {
	ID        string  `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string  `gorm:"size:64;not null;index:idx_conversations_tenant_updated,priority:1;index:idx_conversations_tenant_dossier,priority:1" json:"tenant_id"`
	DossierID *string `gorm:"size:64;index:idx_conversations_tenant_dossier,priority:2" json:"dossier_id"`

	Title     string `gorm:"type:text" json:"title"`
	CreatedBy string `gorm:"size:64;index:idx_conversations_created_by" json:"created_by"`
	State     string `gorm:"size:32;not null;default:active" json:"state"` // ConversationState*

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64 `gorm:"autoUpdateTime;index:idx_conversations_tenant_updated,priority:2,sort:desc" json:"updated_at"`
}

func (Conversation) TableName() string { return "conversations" }

// Message is one turn in a thread.
//
// RunID is nullable: most turns start no mission, and a conversational reply
// is just text. State is streaming|complete|failed because an assistant reply
// is persisted incrementally *while* it streams — the row is the durable
// source of truth, the SSE connection is only a view of it (§4.5b). A client
// that reconnects replays the row and the events after its cursor.
type Message struct {
	ID             string `gorm:"primaryKey;size:64" json:"id"`
	TenantID       string `gorm:"size:64;not null;index:idx_messages_tenant_conv_created,priority:1" json:"tenant_id"`
	ConversationID string `gorm:"size:64;not null;index:idx_messages_tenant_conv_created,priority:2" json:"conversation_id"`

	Role    string `gorm:"size:16;not null" json:"role"` // MessageRole*
	Content string `gorm:"type:text" json:"content"`

	RunID *string `gorm:"size:64;index:idx_messages_run" json:"run_id"`

	// Attachments / Sources / Skills are the composer's selections for this
	// turn: which documents are in context, which corpora to search, which
	// compétences to apply. Open-ended by nature, never queried on.
	Attachments JSONB `gorm:"type:jsonb" json:"attachments,omitempty"`
	Sources     JSONB `gorm:"type:jsonb" json:"sources,omitempty"`
	Skills      JSONB `gorm:"type:jsonb" json:"skills,omitempty"`

	State string `gorm:"size:16;not null;default:complete" json:"state"` // MessageState*

	// Truncated marks an answer the model stopped writing because it ran out
	// of completion budget, not because it had finished. The text that did
	// arrive is kept — it is durable and often useful — but a sentence cut
	// mid-word must never be presented as a finished answer to a lawyer, and
	// that has to survive a reload, so it is a column and not just an event.
	Truncated bool `gorm:"not null;default:false" json:"truncated"`

	// Agent turns only; null on every earlier message, and a null is never
	// read as a pass. AnswerRevisionID is the verified answer revision this
	// text was delivered from, VerificationID its server-created report.
	AgentOutcome     string  `gorm:"size:64" json:"agent_outcome"`
	AnswerRevisionID *string `gorm:"size:64" json:"answer_revision_id"`
	VerificationID   *string `gorm:"size:64" json:"verification_id"`

	CreatedAt int64 `gorm:"autoCreateTime;index:idx_messages_tenant_conv_created,priority:3" json:"created_at"`
	UpdatedAt int64 `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Message) TableName() string { return "messages" }

// ---------------------------------------------------------------------------
// The runner — runs, steps, attempts, events, questions (§4)
// ---------------------------------------------------------------------------

// Run is one agent turn or one ingestion. ConversationID is nullable (an
// ingestion run has no thread); DossierID is what the run operates on.
type Run struct {
	ID             string  `gorm:"primaryKey;size:64" json:"id"`
	TenantID       string  `gorm:"size:64;not null;index:idx_runs_tenant_dossier_created,priority:1" json:"tenant_id"`
	ConversationID *string `gorm:"size:64;index:idx_runs_conversation_created,priority:1" json:"conversation_id"`
	DossierID      string  `gorm:"size:64;index:idx_runs_tenant_dossier_created,priority:2" json:"dossier_id"`
	RequestedBy    string  `gorm:"size:64" json:"requested_by"`

	// WorkflowType + WorkflowVersion identify the code-owned definition that
	// produced this plan (§4.1). Both are stored so an old run stays
	// explainable after the definition changes.
	WorkflowType    string `gorm:"size:64;not null" json:"workflow_type"`
	WorkflowVersion int    `gorm:"not null;default:1" json:"workflow_version"`

	Instruction string `gorm:"type:text" json:"instruction"`
	State       string `gorm:"size:32;not null;default:pending;index:idx_runs_state_updated,priority:1" json:"state"`

	// CancelRequestedAt is a request, not a state: a running step observes it
	// and stops at its next safe point. Nothing is killed mid-flight.
	CancelRequestedAt *int64 `json:"cancel_requested_at"`
	FailureCode       string `gorm:"size:64" json:"failure_code"`

	CreatedAt  int64  `gorm:"autoCreateTime;index:idx_runs_tenant_dossier_created,priority:3,sort:desc;index:idx_runs_conversation_created,priority:2" json:"created_at"`
	UpdatedAt  int64  `gorm:"autoUpdateTime;index:idx_runs_state_updated,priority:2" json:"updated_at"`
	StartedAt  *int64 `json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
}

func (Run) TableName() string { return "runs" }

// RunStep is one unit of work: small, idempotent and leased.
//
// AvailableAt / LeaseExpiresAt / HeartbeatAt are timestamptz, deliberately
// breaking the int64-unix-seconds convention used everywhere else. Claiming is
//
//	WHERE state='queued' AND available_at <= now() ... FOR UPDATE SKIP LOCKED
//
// and the reaper requeues on lease expiry with a conditional update on the old
// lease owner — both compare against the Postgres clock, never a pod clock
// (§4.2, §9). Never hold this row's lock across OCR, S3, pandoc or an LLM call.
type RunStep struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_run_steps_tenant" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;uniqueIndex:idx_run_steps_run_key,priority:1" json:"run_id"`
	StepKey  string `gorm:"size:128;not null;uniqueIndex:idx_run_steps_run_key,priority:2" json:"step_key"`

	Kind  string `gorm:"size:32;not null" json:"kind"` // StepKind*, selects the pool
	State string `gorm:"size:32;not null;default:queued;index:idx_run_steps_claim,priority:1" json:"state"`

	AvailableAt  time.Time `gorm:"type:timestamptz;not null;default:now();index:idx_run_steps_claim,priority:2" json:"available_at"`
	AttemptCount int       `gorm:"not null;default:0" json:"attempt_count"`
	MaxAttempts  int       `gorm:"not null;default:3" json:"max_attempts"`

	// IdempotencyKey is stable across attempts of the same logical work — it
	// is what makes at-least-once execution survivable. Execution is
	// at-least-once, never exactly-once (§2.3): an accepted-but-unrecorded
	// LLM call WILL be retried, and that is a named, accepted failure mode.
	IdempotencyKey string `gorm:"size:255;not null;uniqueIndex:idx_run_steps_idempotency" json:"idempotency_key"`

	LeaseOwner     string     `gorm:"size:128" json:"lease_owner"`
	LeaseExpiresAt *time.Time `gorm:"type:timestamptz;index:idx_run_steps_claim,priority:3" json:"lease_expires_at"`
	HeartbeatAt    *time.Time `gorm:"type:timestamptz" json:"heartbeat_at"`

	Input  JSONB `gorm:"type:jsonb" json:"input,omitempty"`
	Output JSONB `gorm:"type:jsonb" json:"output,omitempty"`

	ErrorCode string `gorm:"size:64" json:"error_code"`
	// ErrorSummary is a short operator-facing message. Never a prompt, never
	// document text, never a presigned URL (§7.4).
	ErrorSummary string `gorm:"type:text" json:"error_summary"`

	// Label is what the plan monitor shows for this step (§11).
	Label string `gorm:"type:text" json:"label"`

	CreatedAt  int64  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt  int64  `gorm:"autoUpdateTime" json:"updated_at"`
	StartedAt  *int64 `json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
}

func (RunStep) TableName() string { return "run_steps" }

// RunStepDep is one edge of the plan DAG. The shape is code (§4.1); only the
// fan-out width is data.
type RunStepDep struct {
	ID              string `gorm:"primaryKey;size:64" json:"id"`
	RunID           string `gorm:"size:64;not null;index:idx_run_step_deps_run" json:"run_id"`
	StepID          string `gorm:"size:64;not null;uniqueIndex:idx_run_step_deps_edge,priority:1" json:"step_id"`
	DependsOnStepID string `gorm:"size:64;not null;uniqueIndex:idx_run_step_deps_edge,priority:2" json:"depends_on_step_id"`
	CreatedAt       int64  `gorm:"autoCreateTime" json:"created_at"`
}

func (RunStepDep) TableName() string { return "run_step_deps" }

// StepAttempt is the audit trail of one try. Retry history in a JSON blob gets
// overwritten; a real table is what makes a failure diagnosable (§9.1(5)).
// ExecutionID is unique and is minted BEFORE dispatch, so an LLM call can
// always be tied back to the attempt that made it (§4.6).
type StepAttempt struct {
	ID        string `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string `gorm:"size:64;not null;index:idx_step_attempts_tenant" json:"tenant_id"`
	RunStepID string `gorm:"size:64;not null;uniqueIndex:idx_step_attempts_step_number,priority:1" json:"run_step_id"`
	Number    int    `gorm:"not null;uniqueIndex:idx_step_attempts_step_number,priority:2" json:"number"`

	ExecutionID string `gorm:"size:64;not null;uniqueIndex:idx_step_attempts_execution" json:"execution_id"`
	WorkerID    string `gorm:"size:128" json:"worker_id"`

	State     string `gorm:"size:32;not null;default:running" json:"state"` // AttemptState*
	Retryable bool   `gorm:"not null;default:true" json:"retryable"`

	ProviderRequestID string `gorm:"size:255" json:"provider_request_id"`
	ErrorCode         string `gorm:"size:64" json:"error_code"`

	StartedAt  int64  `gorm:"autoCreateTime" json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
}

func (StepAttempt) TableName() string { return "step_attempts" }

// RunEvent is the user-visible progress log and the cursor the thread replays
// from. It is inserted in the SAME transaction as the state change it
// describes, which is what makes the cursor trustworthy after a pod restart.
//
// Two columns extend §9's listing, because §4.5b requires one cursor to serve
// both a mission and a plain conversational reply: RunID is nullable, and
// ConversationID is present. A token delta for a conversational reply has no
// run; a step event has no conversation unless the run was started from one.
// Both indexes are (tenant, scope, id) so the cursor is a range scan.
//
// This is progress, not evidence: audit_events is the compliance log. Do not
// merge them, and put no document content in either.
type RunEvent struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_run_events_tenant_run_id,priority:1;index:idx_run_events_tenant_conv_id,priority:1" json:"tenant_id"`

	RunID          *string `gorm:"size:64;index:idx_run_events_tenant_run_id,priority:2" json:"run_id"`
	ConversationID *string `gorm:"size:64;index:idx_run_events_tenant_conv_id,priority:2" json:"conversation_id"`
	RunStepID      *string `gorm:"size:64" json:"run_step_id"`

	Type    string `gorm:"size:64;not null" json:"type"` // Event*
	Payload JSONB  `gorm:"type:jsonb" json:"payload,omitempty"`

	CreatedAt int64 `gorm:"autoCreateTime;index:idx_run_events_tenant_run_id,priority:3;index:idx_run_events_tenant_conv_id,priority:3" json:"created_at"`
}

func (RunEvent) TableName() string { return "run_events" }

// RunQuestion is one human-in-the-loop question. `ask_user` is an ordinary
// step that writes these rows and parks itself in waiting_input; dependents
// stay queued. No timers, no in-memory state, replica-safe by construction
// (§4.4). A skip is recorded as an answer, never as silence.
type RunQuestion struct {
	ID        string `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string `gorm:"size:64;not null;index:idx_run_questions_tenant_run,priority:1" json:"tenant_id"`
	RunID     string `gorm:"size:64;not null;index:idx_run_questions_tenant_run,priority:2" json:"run_id"`
	RunStepID string `gorm:"size:64;not null;uniqueIndex:idx_run_questions_step_key,priority:1" json:"run_step_id"`

	QuestionKey string `gorm:"size:128;not null;uniqueIndex:idx_run_questions_step_key,priority:2" json:"question_key"`
	Prompt      string `gorm:"type:text;not null" json:"prompt"`
	Kind        string `gorm:"size:16;not null;default:single" json:"kind"` // QuestionKind*
	Options     JSONB  `gorm:"type:jsonb" json:"options,omitempty"`

	State string `gorm:"size:32;not null;default:pending" json:"state"` // QuestionState*
	// Required marks a question the agent cannot finish without; skipping
	// it is recorded and limits the work, never answered by invention.
	Required   bool   `gorm:"not null;default:false" json:"required"`
	Answer     JSONB  `gorm:"type:jsonb" json:"answer,omitempty"`
	AnsweredBy string `gorm:"size:64" json:"answered_by"`

	AskedAt    int64  `gorm:"autoCreateTime" json:"asked_at"`
	AnsweredAt *int64 `json:"answered_at"`
}

func (RunQuestion) TableName() string { return "run_questions" }

// Artifact is a published deliverable. Publication is the last step of an
// export: write to a temp key, validate, then insert this row — so a killed
// pod never leaves a half-published artifact visible (§4.3).
//
// TemplateVersion / RendererVersion exist so a document can be reproduced:
// pandoc version, reference-doc hash and template revision travel with it.
type Artifact struct {
	ID        string `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string `gorm:"size:64;not null;index:idx_artifacts_tenant" json:"tenant_id"`
	RunID     string `gorm:"size:64;not null;index:idx_artifacts_run" json:"run_id"`
	DossierID string `gorm:"size:64;index:idx_artifacts_dossier" json:"dossier_id"`

	Kind    string `gorm:"size:64;not null" json:"kind"` // ArtifactKind*
	AssetID string `gorm:"size:64;not null" json:"asset_id"`
	Name    string `gorm:"type:text;not null" json:"name"`

	CharCount       int    `json:"char_count"`
	TemplateVersion string `gorm:"size:64" json:"template_version"`
	RendererVersion string `gorm:"size:64" json:"renderer_version"`

	// Approved records that a lawyer signed off. An unapproved export is
	// stamped "projet — vérification de l'avocat requise" (§13).
	Approved   bool   `gorm:"not null;default:false" json:"approved"`
	ApprovedBy string `gorm:"size:64" json:"approved_by"`
	ApprovedAt *int64 `json:"approved_at"`

	// Agent publication; null on legacy artifacts, which keep their legacy
	// download policy. PublicationKey is the stable identity of one
	// publication (tenant, run, revision hash, format, template and
	// warning-report hashes), so a retry reuses the row.
	DocumentRevisionID *string `gorm:"size:64" json:"document_revision_id"`
	VerificationID     *string `gorm:"size:64" json:"verification_id"`
	PublicationKey     *string `gorm:"size:64;uniqueIndex:idx_artifacts_publication_key" json:"-"`
	Partial            bool    `gorm:"not null;default:false" json:"partial"`
	PieceWarnings      int     `gorm:"not null;default:0" json:"piece_warnings"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (Artifact) TableName() string { return "artifacts" }

// ---------------------------------------------------------------------------
// Cost accounting (§4.6) — a fire-and-forget record loses exactly the
// expensive runs, because it loses them when the pod dies.
// ---------------------------------------------------------------------------

// LLMCall is the durable record of one gateway call. The row is written with
// the ExecutionID minted before dispatch and reconciled with the usage and the
// provider-reported cost once the provider answers.
type LLMCall struct {
	ID            string  `gorm:"primaryKey;size:64" json:"id"`
	TenantID      string  `gorm:"size:64;not null;index:idx_llm_calls_tenant_created,priority:1" json:"tenant_id"`
	RunID         *string `gorm:"size:64;index:idx_llm_calls_run" json:"run_id"`
	RunStepID     *string `gorm:"size:64;index:idx_llm_calls_step" json:"run_step_id"`
	StepAttemptID *string `gorm:"size:64" json:"step_attempt_id"`

	ExecutionID       string `gorm:"size:64;not null;uniqueIndex:idx_llm_calls_execution" json:"execution_id"`
	ProviderRequestID string `gorm:"size:255" json:"provider_request_id"`

	Model   string `gorm:"size:128;not null" json:"model"`
	Purpose string `gorm:"size:32;not null" json:"purpose"`                  // LLMPurpose*
	State   string `gorm:"size:32;not null;default:dispatched" json:"state"` // LLMCallState*

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// CostNanoUSD is the cost the provider reported, in billionths of a US
	// dollar (server-tool fees included). 0 while usage is unknown.
	CostNanoUSD int64 `gorm:"not null;default:0" json:"cost_nano_usd"`
	// UsageStatus is `unknown` when the connection died or no usage trailer
	// arrived. Record it and reconcile later — a character-count estimate is
	// not auditable billing.
	UsageStatus string `gorm:"size:16;not null;default:unknown" json:"usage_status"`

	// Agent attribution: the acting user, the controller dispatch, the exact
	// input manifest, the model the provider actually ran, and the usage
	// counters it returned (cached, reasoning, server-tool).
	ActorID             string `gorm:"size:64" json:"actor_id"`
	AgentDispatch       *int   `json:"agent_dispatch"`
	InputManifestSHA256 string `gorm:"size:64" json:"input_manifest_sha256"`
	ReturnedModel       string `gorm:"size:128" json:"returned_model"`
	UsageDetails        JSONB  `gorm:"type:jsonb" json:"usage_details,omitempty"`

	StartedAt  int64  `gorm:"autoCreateTime;index:idx_llm_calls_tenant_created,priority:2,sort:desc" json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
}

func (LLMCall) TableName() string { return "llm_calls" }

// AuditEvent is security and compliance evidence — who did what to which
// object. Separate from run_events on purpose, and equally free of document
// content (§7.4).
type AuditEvent struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_audit_events_tenant_created,priority:1" json:"tenant_id"`

	ActorID    string `gorm:"size:64;index:idx_audit_events_actor" json:"actor_id"`
	Action     string `gorm:"size:64;not null" json:"action"`
	ObjectType string `gorm:"size:64" json:"object_type"`
	ObjectID   string `gorm:"size:64" json:"object_id"`
	Metadata   JSONB  `gorm:"type:jsonb" json:"metadata,omitempty"`

	CreatedAt int64 `gorm:"autoCreateTime;index:idx_audit_events_tenant_created,priority:2,sort:desc" json:"created_at"`
}

func (AuditEvent) TableName() string { return "audit_events" }

// ---------------------------------------------------------------------------
// Slice 2 — the public legal corpus (§5, §6)
// ---------------------------------------------------------------------------

// LegalDocument is the stable identity of a source item: an article, a
// decision. It carries no text — text belongs to an immutable version.
type LegalDocument struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	Source   string `gorm:"size:32;not null;uniqueIndex:idx_legal_documents_source,priority:1" json:"source"`
	SourceID string `gorm:"size:255;not null;uniqueIndex:idx_legal_documents_source,priority:2" json:"source_id"`

	Kind         string `gorm:"size:32;not null;index:idx_legal_documents_kind" json:"kind"` // article | decision | …
	Jurisdiction string `gorm:"size:64;index:idx_legal_documents_jurisdiction" json:"jurisdiction"`
	CanonicalURL string `gorm:"type:text" json:"canonical_url"`
	// CitationLabel is how a lawyer would write it ("C. trav., art. L. 1234-1").
	CitationLabel string `gorm:"type:text" json:"citation_label"`
	// LicenseCode must survive to the output: Licence Ouverte 2.0 permits
	// commercial reuse WITH attribution (§13).
	LicenseCode string `gorm:"size:64" json:"license_code"`

	FirstSeenAt     int64  `gorm:"autoCreateTime" json:"first_seen_at"`
	SourceUpdatedAt *int64 `json:"source_updated_at"`
}

func (LegalDocument) TableName() string { return "legal_documents" }

// LegalVersion is an immutable snapshot of a document's text.
//
// Legal validity is a half-open interval — valid_from <= matter_date AND
// (valid_to IS NULL OR matter_date < valid_to) — and is kept strictly separate
// from IngestedAt, which is an operational timestamp, not an effective date
// (§6.4). For a decision the interval is meaningless: its authority is
// contextual, so decision/publication date, formation, publication level and
// solution are what carry it.
type LegalVersion struct {
	ID              string `gorm:"primaryKey;size:64" json:"id"`
	LegalDocumentID string `gorm:"size:64;not null;uniqueIndex:idx_legal_versions_doc_version,priority:1" json:"legal_document_id"`
	SourceVersion   string `gorm:"size:255;not null;uniqueIndex:idx_legal_versions_doc_version,priority:2" json:"source_version"`

	Text       string `gorm:"type:text" json:"text"`
	TextSHA256 string `gorm:"size:64;index:idx_legal_versions_sha" json:"text_sha256"`

	ValidFrom *int64 `gorm:"index:idx_legal_versions_validity,priority:1" json:"valid_from"`
	ValidTo   *int64 `gorm:"index:idx_legal_versions_validity,priority:2" json:"valid_to"`

	DecisionDate     *int64 `gorm:"index:idx_legal_versions_decision_date" json:"decision_date"`
	PublicationDate  *int64 `json:"publication_date"`
	Formation        string `gorm:"size:128" json:"formation"`
	PublicationLevel string `gorm:"size:64" json:"publication_level"`
	Solution         string `gorm:"size:128" json:"solution"`
	ECLI             string `gorm:"size:128;index:idx_legal_versions_ecli" json:"ecli"`

	State                 string  `gorm:"size:32;not null;default:vigueur" json:"state"` // LegalVersion* — canonical Légifrance vocabulary
	SupersededByVersionID *string `gorm:"size:64" json:"superseded_by_version_id"`

	SourceMeta JSONB `gorm:"type:jsonb" json:"source_meta,omitempty"`
	IngestedAt int64 `gorm:"autoCreateTime" json:"ingested_at"`
}

func (LegalVersion) TableName() string { return "legal_versions" }

// LegalRelation is a typed edge between two versions. Every row carries
// provenance, confidence, evidence and a verification state, because RRF and
// reranking do NOT detect revirements (§6.3) and an LLM-extracted relation is
// a lead, never ground truth. The product surfaces candidates; it never
// asserts that the law changed.
type LegalRelation struct {
	ID            string `gorm:"primaryKey;size:64" json:"id"`
	FromVersionID string `gorm:"size:64;not null;uniqueIndex:idx_legal_relations_edge,priority:1;index:idx_legal_relations_from" json:"from_version_id"`
	ToVersionID   string `gorm:"size:64;not null;uniqueIndex:idx_legal_relations_edge,priority:2;index:idx_legal_relations_to" json:"to_version_id"`
	Type          string `gorm:"size:32;not null;uniqueIndex:idx_legal_relations_edge,priority:3" json:"type"` // Relation*

	Provenance string  `gorm:"size:32;not null" json:"provenance"` // Provenance*
	Confidence float64 `json:"confidence"`
	Evidence   string  `gorm:"type:text" json:"evidence"`

	VerifiedBy string `gorm:"size:64" json:"verified_by"`
	VerifiedAt *int64 `json:"verified_at"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (LegalRelation) TableName() string { return "legal_relations" }

// LegalChunk is the public retrieval unit: HNSW over the embedding + GIN over
// the tsvector, fused by RRF, then reranked (§6.2). Both of those indexes are
// created by utils.EnsureSearchIndexes — AutoMigrate cannot express either.
//
// SearchVector is a plain tsvector column written by application DML
// (`to_tsvector('french', unaccent($1))`), NOT a generated column and NOT a
// trigger: the no-DDL constraint can create neither (§6.2).
type LegalChunk struct {
	ID             uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	LegalVersionID string `gorm:"size:64;not null;uniqueIndex:idx_legal_chunks_version_ordinal,priority:1" json:"legal_version_id"`
	Ordinal        int    `gorm:"not null;uniqueIndex:idx_legal_chunks_version_ordinal,priority:2" json:"ordinal"`

	Text       string `gorm:"type:text;not null" json:"text"`
	TextSHA256 string `gorm:"size:64" json:"text_sha256"`

	SearchVector string `gorm:"type:tsvector" json:"-"`

	Embedding      *pgvector.HalfVector `gorm:"type:halfvec(1536)" json:"-"`
	EmbeddingModel string               `gorm:"size:128" json:"embedding_model"`
	TokenCount     int                  `json:"token_count"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (LegalChunk) TableName() string { return "legal_chunks" }

// PieceChunk is the private retrieval unit — deliberately a separate table
// from LegalChunk, because the two differ in tenant isolation, retention,
// update cadence, provenance, corpus size and index policy (§6.1).
//
// There is deliberately NO HNSW index here. Private retrieval filters on
// tenant_id + dossier_id and then scores exactly: a dossier is hundreds to a
// few thousand chunks, exact is fast, and it avoids the multitenant ANN trap
// where HNSW returns globally-near neighbours the tenant filter then discards.
type PieceChunk struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	TenantID  string `gorm:"size:64;not null;index:idx_piece_chunks_tenant_dossier,priority:1" json:"tenant_id"`
	DossierID string `gorm:"size:64;not null;index:idx_piece_chunks_tenant_dossier,priority:2" json:"dossier_id"`
	PieceID   string `gorm:"size:64;not null;uniqueIndex:idx_piece_chunks_piece_ordinal,priority:1" json:"piece_id"`
	Ordinal   int    `gorm:"not null;uniqueIndex:idx_piece_chunks_piece_ordinal,priority:2" json:"ordinal"`

	PageFrom int `json:"page_from"`
	PageTo   int `json:"page_to"`

	Text       string `gorm:"type:text;not null" json:"text"`
	TextSHA256 string `gorm:"size:64" json:"text_sha256"`

	SearchVector string `gorm:"type:tsvector" json:"-"`

	Embedding      *pgvector.HalfVector `gorm:"type:halfvec(1536)" json:"-"`
	EmbeddingModel string               `gorm:"size:128" json:"embedding_model"`
	TokenCount     int                  `json:"token_count"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (PieceChunk) TableName() string { return "piece_chunks" }

// Citation freezes what a generated claim rests on: the exact version, the
// supporting quote, the source hash, the as-of date retrieval used, when it
// was retrieved, and who verified it (§6.4).
//
// LegalVersionID and PieceID are explicit nullable columns rather than a
// polymorphic pair, so referential intent survives; exactly one must be set,
// enforced in the application and its tests (§9.1(7)).
//
// No citation is ever generated from model memory alone (§13).
type Citation struct {
	ID        string  `gorm:"primaryKey;size:64" json:"id"`
	TenantID  string  `gorm:"size:64;not null;index:idx_citations_tenant_run,priority:1" json:"tenant_id"`
	RunID     string  `gorm:"size:64;not null;index:idx_citations_tenant_run,priority:2" json:"run_id"`
	RunStepID *string `gorm:"size:64;index:idx_citations_step" json:"run_step_id"`

	// ClaimKey identifies the sentence/assertion this citation supports.
	ClaimKey   string `gorm:"size:128;not null" json:"claim_key"`
	SourceType string `gorm:"size:16;not null" json:"source_type"` // CitationSource*

	LegalVersionID *string `gorm:"size:64;index:idx_citations_legal_version" json:"legal_version_id"`
	PieceID        *string `gorm:"size:64;index:idx_citations_piece" json:"piece_id"`

	Locator  string `gorm:"type:text" json:"locator"`
	PageFrom int    `json:"page_from"`
	PageTo   int    `json:"page_to"`
	// Quote is the exact supporting text. A citation without one is not a
	// citation.
	Quote        string `gorm:"type:text" json:"quote"`
	SourceSHA256 string `gorm:"size:64" json:"source_sha256"`

	// AsOfDate is the matter date retrieval was performed against — never
	// silently swap a cited article to its current wording (§6.4).
	AsOfDate    *int64 `json:"as_of_date"`
	RetrievedAt int64  `gorm:"autoCreateTime" json:"retrieved_at"`

	VerificationState string `gorm:"size:32;not null;default:retrieved" json:"verification_state"` // Citation*
	VerifiedBy        string `gorm:"size:64" json:"verified_by"`
	VerifiedAt        *int64 `json:"verified_at"`

	// Agent citations; null on legacy rows. A binding is scoped to the answer
	// message or a document revision and rests on an evidence span (a web
	// citation also names its web source). Number is the first-use order
	// within that scope; Checks holds []CitationCheck.
	MessageID          *string `gorm:"size:64" json:"message_id"`
	DocumentRevisionID *string `gorm:"size:64" json:"document_revision_id"`
	EvidenceSpanID     *string `gorm:"size:64" json:"evidence_span_id"`
	WebSourceID        *string `gorm:"size:64" json:"web_source_id"`
	Number             int     `gorm:"not null;default:0" json:"number"`
	Checks             JSONB   `gorm:"type:jsonb" json:"checks,omitempty"`
}

func (Citation) TableName() string { return "citations" }

// ---------------------------------------------------------------------------
// Slice 2 — compétences and memory (§8)
// ---------------------------------------------------------------------------

// Skill is the identity of a compétence. The definition lives in an immutable
// SkillVersion; CurrentVersion is a pointer, not the content.
type Skill struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;uniqueIndex:idx_skills_tenant_slug,priority:1" json:"tenant_id"`
	Slug     string `gorm:"size:128;not null;uniqueIndex:idx_skills_tenant_slug,priority:2" json:"slug"`

	Name           string `gorm:"type:text;not null" json:"name"`
	Description    string `gorm:"type:text" json:"description"`
	State          string `gorm:"size:32;not null;default:draft" json:"state"` // SkillState*
	CurrentVersion int    `gorm:"not null;default:0" json:"current_version"`

	CreatedBy string `gorm:"size:64" json:"created_by"`
	CreatedAt int64  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64  `gorm:"autoUpdateTime" json:"updated_at"`
}

func (Skill) TableName() string { return "skills" }

// SkillVersion is immutable. A mutable definition makes an earlier pleading
// unreproducible, and reproducing an earlier pleading is the whole point (§8).
type SkillVersion struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_skill_versions_tenant" json:"tenant_id"`
	SkillID  string `gorm:"size:64;not null;uniqueIndex:idx_skill_versions_skill_version,priority:1" json:"skill_id"`
	Version  int    `gorm:"not null;uniqueIndex:idx_skill_versions_skill_version,priority:2" json:"version"`

	SourceAssetID    *string `gorm:"size:64" json:"source_asset_id"`    // the model act it was extracted from
	ReferenceAssetID *string `gorm:"size:64" json:"reference_asset_id"` // the reference .docx for pandoc

	SchemaVersion  int    `gorm:"not null;default:1" json:"schema_version"`
	Definition     JSONB  `gorm:"type:jsonb" json:"definition,omitempty"`
	ExtractorModel string `gorm:"size:128" json:"extractor_model"`

	CreatedBy string `gorm:"size:64" json:"created_by"`
	CreatedAt int64  `gorm:"autoCreateTime" json:"created_at"`
}

func (SkillVersion) TableName() string { return "skill_versions" }

// MemoryPreference is explicit, inspectable, editable, deletable style memory.
// Provenance distinguishes user-approved from inferred; Nomos never silently
// turns conversation text into permanent firm memory (§8). Scoped per tenant
// and per user — one firm's memory never reaches another.
type MemoryPreference struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;uniqueIndex:idx_memory_prefs_scope_key,priority:1" json:"tenant_id"`
	UserID   string `gorm:"size:64;not null;uniqueIndex:idx_memory_prefs_scope_key,priority:2" json:"user_id"`
	Scope    string `gorm:"size:32;not null;uniqueIndex:idx_memory_prefs_scope_key,priority:3" json:"scope"`
	Key      string `gorm:"size:128;not null;uniqueIndex:idx_memory_prefs_scope_key,priority:4" json:"key"`

	Value      JSONB  `gorm:"type:jsonb" json:"value,omitempty"`
	Provenance string `gorm:"size:32;not null;default:user_approved" json:"provenance"` // MemoryProvenance*
	State      string `gorm:"size:32;not null;default:proposed" json:"state"`           // MemoryState*

	ApprovedAt *int64 `json:"approved_at"`
	ExpiresAt  *int64 `json:"expires_at"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64 `gorm:"autoUpdateTime" json:"updated_at"`
}

func (MemoryPreference) TableName() string { return "memory_preferences" }

// CorpusSync is the resumable cursor of a public-corpus loader. Public, not
// tenant-scoped: legal_* is shared.
type CorpusSync struct {
	ID     string `gorm:"primaryKey;size:64" json:"id"`
	Source string `gorm:"size:32;not null;uniqueIndex:idx_corpus_syncs_source" json:"source"` // Source*

	Cursor    string `gorm:"type:text" json:"cursor"`
	State     string `gorm:"size:32;not null;default:idle" json:"state"` // CorpusSync*
	Stats     JSONB  `gorm:"type:jsonb" json:"stats,omitempty"`
	LastError string `gorm:"type:text" json:"last_error"`

	LastRunAt *int64 `json:"last_run_at"`
	CreatedAt int64  `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt int64  `gorm:"autoUpdateTime" json:"updated_at"`
}

func (CorpusSync) TableName() string { return "corpus_syncs" }

// ---------------------------------------------------------------------------
// Agent runtime. One concept per table: turn
// state, the coverage ledger, evidence, revisions, verification and web
// sources never share a row. Shapes stored as JSONB are typed in
// nomos_agent.go / evidence.go / verification.go.
// ---------------------------------------------------------------------------

// AgentTurn is the durable state of one agent run: who asked, the
// configuration it is pinned to, and the counters the server — never the
// model — enforces.
type AgentTurn struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;uniqueIndex:idx_agent_turns_tenant_request,priority:1;index:idx_agent_turns_tenant_conv,priority:1" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;uniqueIndex:idx_agent_turns_run" json:"run_id"`
	ActorID  string `gorm:"size:64;not null;index:idx_agent_turns_actor" json:"actor_id"`

	// RequestID is the client's retry key, or one the server minted.
	RequestID     string `gorm:"size:64;not null;uniqueIndex:idx_agent_turns_tenant_request,priority:2" json:"request_id"`
	RequestSHA256 string `gorm:"size:64;not null" json:"request_sha256"`

	ConversationID     *string `gorm:"size:64;index:idx_agent_turns_tenant_conv,priority:2" json:"conversation_id"`
	UserMessageID      *string `gorm:"size:64" json:"user_message_id"`
	AssistantMessageID *string `gorm:"size:64" json:"assistant_message_id"`
	DossierID          *string `gorm:"size:64" json:"dossier_id"`

	// Dispatches is incremented in the transaction that reserves a
	// controller dispatch, and only while below MaxDispatches
	// (≤ MaxAgentDispatches).
	Dispatches    int `gorm:"not null;default:0" json:"dispatches"`
	MaxDispatches int `gorm:"not null;default:40" json:"max_dispatches"`
	ReviewCalls   int `gorm:"not null;default:0" json:"review_calls"`
	WebSearches   int `gorm:"not null;default:0" json:"web_searches"`

	// DeadlineAt bounds active work (unix seconds). ParkedAt is set while the
	// turn waits on the lawyer; resuming pushes DeadlineAt by the parked time.
	DeadlineAt int64  `gorm:"not null" json:"deadline_at"`
	ParkedAt   *int64 `json:"parked_at"`

	// Outcome is an AgentOutcome*, empty while the turn runs.
	Outcome string `gorm:"size:64" json:"outcome"`

	ControllerModel string  `gorm:"size:128;not null" json:"controller_model"`
	ReviewModel     string  `gorm:"size:128" json:"review_model"`
	PromptVersion   string  `gorm:"size:64;not null" json:"prompt_version"`
	ToolsetVersion  string  `gorm:"size:64;not null" json:"toolset_version"` // hash of the definitions offered
	SkillVersionID  *string `gorm:"size:64" json:"skill_version_id"`
	TaskContract    JSONB   `gorm:"type:jsonb;not null" json:"task_contract"` // AgentTaskContract

	// DeliveredChars is the publication cursor: code points of the verified
	// answer already appended as message.delta.
	DeliveredChars      int     `gorm:"not null;default:0" json:"delivered_chars"`
	FinalVerificationID *string `gorm:"size:64" json:"final_verification_id"`

	CreatedAt  int64  `gorm:"autoCreateTime;index:idx_agent_turns_tenant_conv,priority:3,sort:desc" json:"created_at"`
	UpdatedAt  int64  `gorm:"autoUpdateTime" json:"updated_at"`
	FinishedAt *int64 `json:"finished_at"`
}

func (AgentTurn) TableName() string { return "agent_turns" }

// AgentReadReceipt is the coverage ledger: one exact range of one piece
// segment returned by a tool call. It counts as read only once
// ConsumedDispatch is set — the accepted controller dispatch whose input
// contained that exact text. Fetched, summarised or context-dropped text
// never counts.
type AgentReadReceipt struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_agent_read_receipts_tenant_run_piece,priority:1" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;index:idx_agent_read_receipts_tenant_run_piece,priority:2;uniqueIndex:idx_agent_read_receipts_call_range,priority:1" json:"run_id"`
	PieceID  string `gorm:"size:64;not null;index:idx_agent_read_receipts_tenant_run_piece,priority:3;uniqueIndex:idx_agent_read_receipts_call_range,priority:3" json:"piece_id"`

	// SourceSHA256 (the original's hash) and ExtractionVersion pin the text
	// the range was cut from; a change to either is source mutation.
	SourceSHA256      string `gorm:"size:64;not null" json:"source_sha256"`
	ExtractionVersion string `gorm:"size:64;not null" json:"extraction_version"`

	SegmentID   string `gorm:"size:128;not null;uniqueIndex:idx_agent_read_receipts_call_range,priority:4" json:"segment_id"`
	StartOffset int    `gorm:"not null;uniqueIndex:idx_agent_read_receipts_call_range,priority:5" json:"start_offset"`
	EndOffset   int    `gorm:"not null;uniqueIndex:idx_agent_read_receipts_call_range,priority:6" json:"end_offset"`
	TextSHA256  string `gorm:"size:64;not null" json:"text_sha256"`

	ToolCallID       string `gorm:"size:64;not null;uniqueIndex:idx_agent_read_receipts_call_range,priority:2" json:"tool_call_id"`
	RunStepID        string `gorm:"size:64;not null" json:"run_step_id"`
	ConsumedDispatch *int   `json:"consumed_dispatch"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (AgentReadReceipt) TableName() string { return "agent_read_receipts" }

// AgentEvidenceSpan is one exact passage a tool returned: the only thing a
// claim may cite. Exactly one of PieceID / LegalVersionID / WebSourceID is
// set, matching SourceType. SpanKey (EvidenceSpanKey) makes it idempotent.
type AgentEvidenceSpan struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_agent_evidence_spans_tenant_run,priority:1" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;index:idx_agent_evidence_spans_tenant_run,priority:2;uniqueIndex:idx_agent_evidence_spans_run_key,priority:1" json:"run_id"`
	SpanKey  string `gorm:"size:64;not null;uniqueIndex:idx_agent_evidence_spans_run_key,priority:2" json:"span_key"`

	SourceType     string  `gorm:"size:16;not null" json:"source_type"` // CitationSource*: piece|legifrance|judilibre|web
	PieceID        *string `gorm:"size:64" json:"piece_id"`
	LegalVersionID *string `gorm:"size:64" json:"legal_version_id"`
	WebSourceID    *string `gorm:"size:64" json:"web_source_id"`

	Locator      JSONB  `gorm:"type:jsonb;not null" json:"locator"` // SourceLocator
	Quote        string `gorm:"type:text;not null" json:"quote"`
	SourceSHA256 string `gorm:"size:64;not null" json:"source_sha256"`

	// Retrieval provenance and temporal basis: which call returned it, and
	// the applicability date or case-law cutoff it was retrieved against.
	RetrievalTool string `gorm:"size:64;not null" json:"retrieval_tool"`
	ToolCallID    string `gorm:"size:64;not null" json:"tool_call_id"`
	AsOfDate      *int64 `json:"as_of_date"`

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (AgentEvidenceSpan) TableName() string { return "agent_evidence_spans" }

// AgentDocumentRevision is one immutable revision of an agent-authored
// document — the answer included (kind `answer`). Nothing updates a row: an
// edit inserts revision N+1. CreatedByCallID makes creation idempotent.
type AgentDocumentRevision struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_agent_doc_revisions_tenant_run,priority:1" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;index:idx_agent_doc_revisions_tenant_run,priority:2;uniqueIndex:idx_agent_doc_revisions_key,priority:1;uniqueIndex:idx_agent_doc_revisions_call,priority:1" json:"run_id"`

	DocumentKey      string  `gorm:"size:128;not null;uniqueIndex:idx_agent_doc_revisions_key,priority:2" json:"document_key"`
	Revision         int     `gorm:"not null;uniqueIndex:idx_agent_doc_revisions_key,priority:3" json:"revision"`
	ParentRevisionID *string `gorm:"size:64" json:"parent_revision_id"`

	Kind  string `gorm:"size:32;not null" json:"kind"` // AgentOutput*
	Title string `gorm:"type:text" json:"title"`

	ContentSHA256  string  `gorm:"size:64;not null" json:"content_sha256"` // DocumentContentSHA256
	SchemaVersion  int     `gorm:"not null;default:1" json:"schema_version"`
	Body           JSONB   `gorm:"type:jsonb;not null" json:"body"` // DocumentRevisionBody
	SkillVersionID *string `gorm:"size:64" json:"skill_version_id"`

	CreatedByCallID string `gorm:"size:64;not null;uniqueIndex:idx_agent_doc_revisions_call,priority:2" json:"created_by_call_id"`
	CreatedAt       int64  `gorm:"autoCreateTime" json:"created_at"`
}

func (AgentDocumentRevision) TableName() string { return "agent_document_revisions" }

// AgentVerification is one server-created verification of one revision,
// bound to the exact hashes it checked. A null legacy verification is never
// reinterpreted as a pass.
type AgentVerification struct {
	ID                 string `gorm:"primaryKey;size:64" json:"id"`
	TenantID           string `gorm:"size:64;not null;index:idx_agent_verifications_tenant_run,priority:1" json:"tenant_id"`
	RunID              string `gorm:"size:64;not null;index:idx_agent_verifications_tenant_run,priority:2" json:"run_id"`
	DocumentRevisionID string `gorm:"size:64;not null;uniqueIndex:idx_agent_verifications_target,priority:1" json:"document_revision_id"`

	ContentSHA256   string `gorm:"size:64;not null;uniqueIndex:idx_agent_verifications_target,priority:2" json:"content_sha256"`
	ScopeSHA256     string `gorm:"size:64;not null;uniqueIndex:idx_agent_verifications_target,priority:3" json:"scope_sha256"`
	EvidenceSHA256  string `gorm:"size:64;not null;uniqueIndex:idx_agent_verifications_target,priority:4" json:"evidence_sha256"`
	VerifierVersion string `gorm:"size:64;not null;uniqueIndex:idx_agent_verifications_target,priority:5" json:"verifier_version"`

	Result          string `gorm:"size:16;not null" json:"result"` // Verification*
	DownloadAllowed bool   `gorm:"not null;default:false" json:"download_allowed"`
	Warnings        int    `gorm:"not null;default:0" json:"warnings"`
	Blocking        int    `gorm:"not null;default:0" json:"blocking"`
	Report          JSONB  `gorm:"type:jsonb;not null" json:"report"` // VerificationReport

	CreatedAt int64 `gorm:"autoCreateTime" json:"created_at"`
}

func (AgentVerification) TableName() string { return "agent_verifications" }

// AgentWebSource is one URL a search_web call actually returned, with the
// excerpt the provider annotated. A generated search summary is not source
// text; a URL without excerpt is a lead, never quote-backed support.
type AgentWebSource struct {
	ID       string `gorm:"primaryKey;size:64" json:"id"`
	TenantID string `gorm:"size:64;not null;index:idx_agent_web_sources_tenant_run,priority:1" json:"tenant_id"`
	RunID    string `gorm:"size:64;not null;index:idx_agent_web_sources_tenant_run,priority:2;uniqueIndex:idx_agent_web_sources_run_url_content,priority:1" json:"run_id"`

	URL string `gorm:"type:text;not null" json:"url"` // canonical
	// URLSHA256 carries the uniqueness: a URL can outgrow a btree entry.
	URLSHA256     string `gorm:"size:64;not null;uniqueIndex:idx_agent_web_sources_run_url_content,priority:2" json:"url_sha256"`
	Host          string `gorm:"size:255" json:"host"`
	Title         string `gorm:"type:text" json:"title"`
	Content       string `gorm:"type:text" json:"content"` // the returned excerpt or text
	ContentSHA256 string `gorm:"size:64;not null;uniqueIndex:idx_agent_web_sources_run_url_content,priority:3" json:"content_sha256"`
	Extent        string `gorm:"size:16;not null" json:"extent"` // WebExtent*

	// Annotation is the provider's url_citation annotation, kept as returned.
	Annotation JSONB  `gorm:"type:jsonb" json:"annotation,omitempty"`
	ToolCallID string `gorm:"size:64;not null" json:"tool_call_id"`

	RetrievedAt int64 `gorm:"autoCreateTime" json:"retrieved_at"`
}

func (AgentWebSource) TableName() string { return "agent_web_sources" }

// NomosModels is the migration set, in dependency-free order (there are no
// foreign keys — every relation is an indexed id column). utils.initDB passes
// it straight to AutoMigrate, so adding a model here is the whole registration
// step.
//
// AutoMigrate only ADDS. It never drops, renames or narrows, and GORM's own
// docs warn it may still change some column types and nullability. Treat every
// edit below as a potentially irreversible production operation (§2.4).
func NomosModels() []any {
	return []any{
		// Slice 1
		&Firm{}, &Asset{}, &Dossier{}, &Piece{}, &PiecePartie{},
		&Conversation{}, &Message{},
		&Run{}, &RunStep{}, &RunStepDep{}, &StepAttempt{}, &RunEvent{}, &RunQuestion{},
		&Artifact{}, &LLMCall{}, &AuditEvent{},
		// Slice 2
		&LegalDocument{}, &LegalVersion{}, &LegalRelation{},
		&LegalChunk{}, &PieceChunk{}, &Citation{},
		&Skill{}, &SkillVersion{}, &MemoryPreference{}, &CorpusSync{},
		// Agent runtime
		&AgentTurn{}, &AgentReadReceipt{}, &AgentEvidenceSpan{},
		&AgentDocumentRevision{}, &AgentVerification{}, &AgentWebSource{},
	}
}
