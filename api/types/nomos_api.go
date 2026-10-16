package types

// Request / response shapes for the Nomos API surface (docs/PLAN.md §10).
//
// Error bodies are always gin.H{"detail": "<message>"} — the shape the
// frontend already handles. Every list response carries a Total from Count:
// "bound every list" is a rule, and ListItems' unbounded scan is a demo, not a
// pattern.

// ---------------------------------------------------------------------------
// Pagination and cursors
// ---------------------------------------------------------------------------

// ListQuery is the shared offset pagination for list endpoints. Bind with
// c.ShouldBindQuery.
type ListQuery struct {
	Limit  int `form:"limit" json:"limit"`
	Offset int `form:"offset" json:"offset"`
}

// Normalized clamps the page window: 50 by default, 200 at most, never
// negative. Callers must use this rather than trusting the query string.
func (q ListQuery) Normalized() (limit, offset int) {
	limit, offset = q.Limit, q.Offset
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// EventsQuery is the cursor for GET …/events (§4.5) and the replay position
// for GET …/stream (§4.5b). After is a durable run_events.id, never a
// timestamp — that is what makes the cursor resumable across a pod restart.
type EventsQuery struct {
	After uint64 `form:"after" json:"after"`
	Limit int    `form:"limit" json:"limit"`
}

// Normalized clamps the event page: 100 by default, 500 at most.
func (q EventsQuery) Normalized() (after uint64, limit int) {
	limit = q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	return q.After, limit
}

// ListResponse is the envelope every paginated list returns.
type ListResponse struct {
	Items  any   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

// EventsResponse is the envelope for a cursor poll. NextAfter is the cursor to
// send on the following poll; it equals After when nothing new arrived.
type EventsResponse struct {
	Events    []RunEvent `json:"events"`
	NextAfter uint64     `json:"next_after"`
}

// ---------------------------------------------------------------------------
// Dossiers and pieces
// ---------------------------------------------------------------------------

// DossierCreateRequest is the body for POST /api/dossiers.
type DossierCreateRequest struct {
	Title           string `json:"title" binding:"required"`
	ClientReference string `json:"client_reference"`
	MatterType      string `json:"matter_type"`
	// AutoTitle makes Title a placeholder that the thread's next turn
	// replaces with a generated name (Dossier.TitlePending).
	AutoTitle bool `json:"auto_title"`
}

// DossierDetail is GET /api/dossiers/:id — the dossier plus its counters.
type DossierDetail struct {
	Dossier
	PieceCount    int64 `json:"piece_count"`
	ApprovedCount int64 `json:"approved_count"`
	RunCount      int64 `json:"run_count"`
}

// PieceUpdateRequest is the lawyer correction for PATCH /api/pieces/:id. Every
// proposed field is editable before export (§7.1) — that is the acceptance
// criterion, not a nicety.
type PieceUpdateRequest struct {
	ProposedName        *string `json:"proposed_name"`
	FinalName           *string `json:"final_name"`
	Ordinal             *int    `json:"ordinal"`
	DocumentDate        *int64  `json:"document_date"`
	DatePrecision       *string `json:"date_precision"`
	PieceKind           *string `json:"piece_kind"`
	Title               *string `json:"title"`
	ClassificationState *string `json:"classification_state"`
}

// PieceUploadResponse is the result of POST /api/dossiers/:id/pieces. Rejected
// carries the files that were refused and why — an encrypted PDF or a
// mismatched magic-byte sniff is reported, never silently dropped (§7.1).
type PieceUploadResponse struct {
	Accepted []Piece          `json:"accepted"`
	Rejected []RejectedUpload `json:"rejected"`
	RunID    string           `json:"run_id,omitempty"`
}

// RejectedUpload explains one refused file.
type RejectedUpload struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// DownloadResponse is the presigned-URL envelope shared by piece and artifact
// downloads. The bucket is private; URLs are short-lived and never logged.
type DownloadResponse struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	ExpiresIn int    `json:"expires_in"`
}

// ExportRequest is the body for POST /api/dossiers/:id/export.
type ExportRequest struct {
	// TemplateVersion pins the bordereau template; empty means the current
	// controlled default. Slice 1 ships exactly one.
	TemplateVersion string `json:"template_version"`
	// Approve records that the lawyer signed off on the pieces as they
	// stand. Without it the export is stamped
	// "projet — vérification de l'avocat requise" (§13).
	Approve bool `json:"approve"`
}

// ---------------------------------------------------------------------------
// Conversations — the primary entry point (§11)
// ---------------------------------------------------------------------------

// ConversationCreateRequest is the body for POST /api/conversations. Both
// fields are optional: a thread can be created empty, or with its first turn
// in the same call (the composer's normal path), and it may or may not be
// bound to a dossier collection.
type ConversationCreateRequest struct {
	Title     string                `json:"title"`
	DossierID *string               `json:"dossier_id"`
	Message   *MessageCreateRequest `json:"message"`
	// RequestID is the client's retry key for a creation with a first turn
	// (message.request_id is read when this is empty): a retry with the same
	// key and body returns the thread already created.
	RequestID string `json:"request_id"`
}

// ConversationUpdateRequest is PATCH /api/conversations/:id: rename, or
// bind/unbind the dossier collection. DossierID is a double pointer so an
// explicit JSON `null` unbinds the collection while an absent key leaves it
// alone — the two mean different things here.
type ConversationUpdateRequest struct {
	Title     *string  `json:"title"`
	DossierID **string `json:"dossier_id"`
	State     *string  `json:"state"`
}

// ConversationDetail is GET /api/conversations/:id — the whole thread: the
// messages, the runs they started, and the artifacts those produced. The
// client renders plan monitors, clarification cards and artifacts as blocks
// inside an assistant turn (§11), so they all arrive together.
type ConversationDetail struct {
	Conversation
	Messages  []Message    `json:"messages"`
	Runs      []RunSummary `json:"runs"`
	Artifacts []Artifact   `json:"artifacts"`
	// Cursor is the newest run_events.id visible in this thread; the client
	// opens the stream (or the poll) from here.
	Cursor uint64 `json:"cursor"`
	// RequestID is set on the answer to a creation with a first turn: the
	// client's retry key, or the one the server minted.
	RequestID string `json:"request_id,omitempty"`
}

// MessageCreateRequest is POST /api/conversations/:id/messages — one turn from
// the composer. It may start a run; the response says whether it did.
type MessageCreateRequest struct {
	Content string `json:"content" binding:"required"`
	// AttachmentIDs are piece / asset ids the composer marked as in-context.
	AttachmentIDs []string `json:"attachment_ids"`
	// SkillSlugs are the compétences the `/` menu selected.
	SkillSlugs []string `json:"skill_slugs"`
	// DossierID binds the thread to a collection on the fly, when the user
	// attached documents from the home composer.
	DossierID *string `json:"dossier_id"`
	// RequestID is the client's retry key: a retry with the same key and
	// body returns the turn already committed; the same key with another
	// body is a 409.
	RequestID string `json:"request_id"`
}

// MessageCreateResponse is what the composer gets back immediately. The
// assistant message is created in `streaming` state before any token exists,
// so the client can open the stream against a row that is already durable.
type MessageCreateResponse struct {
	UserMessage      Message `json:"user_message"`
	AssistantMessage Message `json:"assistant_message"`
	RunID            *string `json:"run_id"`
	// Cursor is where the client should start streaming/polling from.
	Cursor uint64 `json:"cursor"`
	// Agent is set when the turn entered the agent runtime.
	Agent *AgentTurnStatus `json:"agent,omitempty"`
	// RequestID is the agent turn's retry key: the client's, or the one the
	// server minted.
	RequestID string `json:"request_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// RunSummary is the compact run shape embedded in a conversation thread.
type RunSummary struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	WorkflowType    string `json:"workflow_type"`
	WorkflowVersion int    `json:"workflow_version"`
	StepsTotal      int    `json:"steps_total"`
	StepsDone       int    `json:"steps_done"`
	CreatedAt       int64  `json:"created_at"`
}

// RunDetail is GET /api/runs/:id — the plan the monitor renders. Steps arrive
// with their dependency edges so parallel fan-out can be drawn as parallel,
// and with attempt counts so a retried step shows it (§11).
type RunDetail struct {
	Run
	Steps     []RunStep     `json:"steps"`
	Deps      []RunStepDep  `json:"deps"`
	Questions []RunQuestion `json:"questions"`
	Artifacts []Artifact    `json:"artifacts"`
	Cursor    uint64        `json:"cursor"`
}

// AnswersRequest is POST /api/runs/:id/answers — the HITL reply (§4.4).
type AnswersRequest struct {
	Answers []QuestionAnswer `json:"answers" binding:"required"`
}

// QuestionAnswer is one answered (or skipped) clarification. A skip is
// recorded as an answer, never as silence — the run must be able to explain
// what the lawyer was asked and what they chose not to decide.
type QuestionAnswer struct {
	QuestionKey string   `json:"question_key" binding:"required"`
	Values      []string `json:"values"`
	Text        string   `json:"text"`
	Skipped     bool     `json:"skipped"`
}

// CancelResponse acknowledges POST /api/runs/:id/cancel. Cancellation is a
// request observed at the next safe point, not a kill.
type CancelResponse struct {
	RunID             string `json:"run_id"`
	State             string `json:"state"`
	CancelRequestedAt int64  `json:"cancel_requested_at"`
}

// ---------------------------------------------------------------------------
// Retrieval (slice 2)
// ---------------------------------------------------------------------------

// SearchRequest is POST /api/search.
type SearchRequest struct {
	Query string `json:"query" binding:"required"`
	// Corpus is CorpusPublic, CorpusPrivate or CorpusBoth.
	Corpus string `json:"corpus"`
	// DossierID scopes a private search. Required when Corpus includes
	// private — a private read without a dossier scope is a bug.
	DossierID *string `json:"dossier_id"`
	// AsOfDate is the matter date temporal filtering runs against (§6.4).
	// Zero means "today", which is only correct for a current dispute.
	AsOfDate int64 `json:"as_of_date"`
	Limit    int   `json:"limit"`
}

// SearchResponse carries the fused, reranked hits.
type SearchResponse struct {
	Results []SearchHit `json:"results"`
	// Reranked reports whether the rerank pass actually ran; when the
	// gateway is unavailable the RRF order is returned as-is and the UI must
	// not claim otherwise.
	Reranked bool `json:"reranked"`
}

// SearchHit is one retrieved chunk with the provenance a citation needs.
// RRF and reranking establish topical relevance only: neither says whether a
// decision was overruled, distinguished or superseded (§6.3).
type SearchHit struct {
	Corpus     string  `json:"corpus"`
	ChunkID    uint64  `json:"chunk_id"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	RerankRank int     `json:"rerank_rank"`

	// Public provenance.
	LegalVersionID string `json:"legal_version_id,omitempty"`
	CitationLabel  string `json:"citation_label,omitempty"`
	CanonicalURL   string `json:"canonical_url,omitempty"`
	LicenseCode    string `json:"license_code,omitempty"`
	ValidFrom      *int64 `json:"valid_from,omitempty"`
	ValidTo        *int64 `json:"valid_to,omitempty"`

	// Private provenance.
	PieceID  string `json:"piece_id,omitempty"`
	PageFrom int    `json:"page_from,omitempty"`
	PageTo   int    `json:"page_to,omitempty"`
}

// ---------------------------------------------------------------------------
// Compétences and memory (slice 2)
// ---------------------------------------------------------------------------

// SkillCreateRequest is POST /api/skills — create a compétence, optionally by
// extracting it from a model act. The definition is stored as an immutable
// version (§8).
type SkillCreateRequest struct {
	Slug             string  `json:"slug" binding:"required"`
	Name             string  `json:"name" binding:"required"`
	Description      string  `json:"description"`
	SourceAssetID    *string `json:"source_asset_id"`
	ReferenceAssetID *string `json:"reference_asset_id"`
	Definition       JSONB   `json:"definition"`
}

// SkillDetail is GET /api/skills/:id — identity plus its version history.
type SkillDetail struct {
	Skill
	Versions []SkillVersion `json:"versions"`
}

// MemoryUpsertRequest is POST /api/memory. Provenance defaults to
// user_approved: Nomos does not silently turn conversation text into permanent
// firm memory, so an inferred entry is created in `proposed` state and must be
// approved before it is used (§8).
type MemoryUpsertRequest struct {
	Scope      string `json:"scope" binding:"required"`
	Key        string `json:"key" binding:"required"`
	Value      JSONB  `json:"value"`
	Provenance string `json:"provenance"`
	ExpiresAt  *int64 `json:"expires_at"`
}

// ---------------------------------------------------------------------------
// Admin — corpus
// ---------------------------------------------------------------------------

// CorpusSyncRequest is POST /api/admin/corpus/sync.
type CorpusSyncRequest struct {
	Source string `json:"source" binding:"required"` // Source*
	// Full forces a rebuild from the start rather than resuming the cursor.
	Full bool `json:"full"`
}

// CorpusStatusResponse is GET /api/admin/corpus/status — the sync cursors plus
// the corpus counters an admin needs to see that §5.3's sizing still holds.
type CorpusStatusResponse struct {
	Syncs      []CorpusSync `json:"syncs"`
	Documents  int64        `json:"documents"`
	Versions   int64        `json:"versions"`
	Chunks     int64        `json:"chunks"`
	Embedded   int64        `json:"embedded"`
	IndexReady bool         `json:"index_ready"`
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// DeepHealthResponse is GET /api/health?deep=1. The shallow probe stays green
// while Postgres, S3, the gateway or the runner is dead (§2.5); this one does
// not. Status is "ok" only when every required check passed.
type DeepHealthResponse struct {
	Status   string            `json:"status"`
	Checks   map[string]Check  `json:"checks"`
	Features map[string]bool   `json:"features"`
	Versions map[string]string `json:"versions"`
}

// Check is one dependency probe result. LatencyMS is a shape, not a secret.
type Check struct {
	Status    string `json:"status"` // ok | degraded | down | skipped
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}
