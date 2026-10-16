/**
 * Agent wire contracts — the TypeScript mirror of api/types/nomos_agent.go,
 * evidence.go and verification.go. Change these only
 * together with the Go types.
 *
 * Timestamps are unix seconds. Money is integer minor units (a JS number is
 * exact up to 2^53, far beyond any amount in a dossier) — never a float.
 */

// ---------------------------------------------------------------------------
// Limits and identities
// ---------------------------------------------------------------------------

/** Hard ceiling on controller-model dispatches per turn. */
export const MAX_AGENT_DISPATCHES = 40;
/** The public intent line every tool call carries. */
export const MAX_PROGRESS_NOTE_CHARS = 140;
/** The durable thread event behind one activity row. */
export const AGENT_ACTIVITY_EVENT = 'agent.activity';

export type AgentToolName =
	| 'list_pieces'
	| 'search_pieces'
	| 'read_piece'
	| 'search_law'
	| 'read_law'
	| 'search_caselaw'
	| 'read_decision'
	| 'search_web'
	| 'read_evidence'
	| 'list_competences'
	| 'load_competence'
	| 'ask_lawyer'
	| 'write_document'
	| 'edit_document'
	| 'read_document'
	| 'calculate'
	| 'verify_output'
	| 'review_output'
	| 'render_document'
	| 'export_dossier'
	| 'submit_result';

export type AgentOutcome =
	| 'completed'
	| 'partial'
	| 'verification_refused'
	| 'agent_turn_limit'
	| 'agent_deadline'
	| 'no_progress'
	| 'cancelled'
	| 'agent_failed'
	| 'submission_required'
	| 'invalid_tool_call'
	| 'model_refusal'
	| 'model_truncated';

/** The agent part of a turn: MessageCreateResponse.agent. */
export interface AgentTurnStatus {
	run_id: string;
	dispatches: number;
	max_dispatches: number;
	/** Empty while the turn runs. */
	outcome?: AgentOutcome;
	deadline_at: number;
	/** Waiting on the lawyer. */
	parked: boolean;
}

/** Body of a refused intake (409 active_turn or request_conflict, 422 otherwise). */
export interface AgentIntakeErrorBody {
	detail: string;
	code: 'active_turn' | 'request_conflict' | 'invalid_competence';
	/** The run already in progress, for active_turn. */
	run_id?: string;
}

// ---------------------------------------------------------------------------
// Activity rows (owner decision 1)
// ---------------------------------------------------------------------------

/** `running` when a call starts; rows sharing a call_id replace each other. */
export type ActivityStatus = 'running' | 'ok' | 'empty' | 'error';

export type ActivityObjectKind =
	| 'pieces'
	| 'piece'
	| 'query'
	| 'law'
	| 'decision'
	| 'evidence'
	| 'competence'
	| 'question'
	| 'document'
	| 'verification'
	| 'review'
	| 'calculation'
	| 'artifact'
	| 'submission';

/** What a call touched — never raw JSON, arguments or hidden reasoning. */
export interface AgentActivityObject {
	kind: ActivityObjectKind;
	label: string;
	detail?: string;
}

/** Present keys only; a real zero is sent as 0. */
export interface AgentActivityCounts {
	results?: number;
	total?: number;
	segments?: number;
	warnings?: number;
	blocking?: number;
	questions?: number;
}

/** Payload of an `agent.activity` event. Rows group by `dispatch`. */
export interface AgentActivityPayload {
	dispatch: number;
	call_id: string;
	tool: AgentToolName;
	/** Shown exactly as written; ≤140 characters. */
	progress_note: string;
	status: ActivityStatus;
	object: AgentActivityObject;
	counts?: AgentActivityCounts;
	at: number;
}

// ---------------------------------------------------------------------------
// Dates and money
// ---------------------------------------------------------------------------

export type DatePrecision = 'day' | 'month' | 'year' | 'unknown';

/** Month and day are absent when unknown — never a manufactured 1st. */
export interface PartialDate {
	precision: DatePrecision;
	year?: number;
	month?: number;
	day?: number;
}

export type DateRole = 'event' | 'document' | 'receipt';

export interface ClaimDate extends PartialDate {
	role: DateRole;
}

export type MoneyQualifier = 'gross' | 'net' | 'ht' | 'ttc';

export interface Money {
	minor_units: number;
	/** ISO 4217, e.g. "EUR". */
	currency: string;
	qualifier?: MoneyQualifier;
}

// ---------------------------------------------------------------------------
// Locators
// ---------------------------------------------------------------------------

/**
 * `start`/`end` are half-open code-point offsets into the locator's text
 * unit: page text (pdf), paragraph (docx, decision), cell (xlsx), article
 * version (legislation), returned excerpt (web). DOCX has no page number.
 * `text_version` names the pièce text representation the offsets count in;
 * a locator without one predates it.
 */
interface LocatorBase {
	segment_id?: string;
	start: number;
	end: number;
	text_version?: string;
}

export type SourceLocator =
	| (LocatorBase & { kind: 'pdf'; page: number; paragraph?: number })
	| (LocatorBase & { kind: 'docx'; paragraph: number })
	/** The worksheet's real name and the cell; an early locator may carry neither. */
	| (LocatorBase & { kind: 'xlsx'; sheet?: string; cell?: string })
	| (LocatorBase & {
			kind: 'legislation';
			article_id?: string;
			version_id: string;
			paragraph?: number;
	  })
	| (LocatorBase & { kind: 'decision'; decision_id: string; paragraph?: number })
	| (LocatorBase & { kind: 'web'; url: string });

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

export type PieceCoverageState =
	| 'complete'
	| 'partial'
	| 'unread'
	| 'pending'
	| 'unreadable'
	| 'excluded';

export interface SegmentCounts {
	expected: number;
	consumed: number;
	missing: number;
	pending: number;
	unreadable: number;
	excluded: number;
}

export interface PieceCoverage {
	piece_id: string;
	ordinal: number;
	name: string;
	state: PieceCoverageState;
	/** What a segment is for this piece. */
	unit: 'page' | 'paragraph' | 'cell';
	segments: SegmentCounts;
	/** The source changed after it was read. */
	mutated: boolean;
}

/** Server-derived from read receipts; `complete` is never model-asserted. */
export interface CoverageReport {
	run_id: string;
	scope_sha256: string;
	exhaustive: boolean;
	complete: boolean;
	total_pieces: number;
	fully_read: number;
	segments: SegmentCounts;
	pieces: PieceCoverage[];
	computed_at: number;
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

export type VerificationSeverity = 'warning' | 'blocking';
/** `warnings` is downloadable but never shown as a green verified state. */
export type VerificationResult = 'passed' | 'warnings' | 'blocked';

export type VerificationIssueCode =
	| 'evidence_unauthorized'
	| 'evidence_unknown'
	| 'source_mutated'
	| 'revision_hash_mismatch'
	| 'structure_invalid'
	| 'piece_quote_not_found'
	| 'piece_date_not_found'
	| 'piece_amount_mismatch'
	| 'piece_calculation_unsupported'
	| 'piece_metadata_only'
	| 'legal_not_retrieved'
	| 'legal_unresolved'
	| 'legal_quote_not_found'
	| 'legal_version_not_applicable'
	| 'caselaw_after_cutoff'
	| 'legal_reference_uncited'
	| 'legal_official_mismatch'
	| 'legal_official_unavailable'
	| 'web_excerpt_missing'
	| 'web_url_not_returned'
	| 'coverage_incomplete'
	| 'coverage_pending'
	| 'competence_section_missing'
	| 'competence_clause_missing'
	| 'lawyer_decision_unresolved'
	| 'review_missing'
	| 'artifact_fidelity_mismatch'
	| 'artifact_warning_missing';

// ---------------------------------------------------------------------------
// Citations (§7.5)
// ---------------------------------------------------------------------------

export type CitationSourceType = 'piece' | 'legifrance' | 'judilibre' | 'web';

export type CitationCheckKind =
	| 'source_resolved'
	| 'quote_matched'
	| 'temporal_valid'
	| 'official_matched'
	| 'semantic_support';

export type CitationCheckStatus = 'passed' | 'failed' | 'unavailable' | 'not_checked';

/** One separate check result; none implies another. */
export interface CitationCheck {
	check: CitationCheckKind;
	status: CitationCheckStatus;
	checked_at?: number;
}

interface CitationBase {
	id: string;
	/** Server-assigned order of first use. */
	number: number;
	claim_ids: string[];
	message_id: string | null;
	document_revision_id: string | null;
	evidence_span_id: string;
	label: string;
	/** Exact supporting text — render as text, never as HTML. */
	quote: string;
	locator: SourceLocator;
	source_sha256: string;
	retrieved_at: number;
	checked_at: number | null;
	checks: CitationCheck[];
	warning_codes: VerificationIssueCode[];
	/** Only ever set by a lawyer's action. */
	verified_by?: string;
	verified_at?: number;
}

export interface PieceCitation extends CitationBase {
	source_type: 'piece';
	piece_id: string;
	piece_ordinal?: number;
	filename?: string;
}

/** Missing source metadata stays missing — nothing is invented. */
export interface LegifranceCitation extends CitationBase {
	source_type: 'legifrance';
	article_id?: string;
	article_label?: string;
	stored_version_id?: string;
	official_version_id?: string;
	valid_from?: number;
	/** Absent for a version still in force. */
	valid_to?: number;
	applicability_date?: number;
	url?: string;
}

export interface JudilibreCitation extends CitationBase {
	source_type: 'judilibre';
	decision_id?: string;
	court?: string;
	chamber?: string;
	decision_date?: number;
	ecli?: string;
	pourvoi?: string;
	url?: string;
}

/** No legal-authority badge. `excerpt` means only the excerpt was available. */
export interface WebCitation extends CitationBase {
	source_type: 'web';
	url: string;
	host?: string;
	title?: string;
	extent: 'excerpt' | 'full_text';
}

export type AgentCitation = PieceCitation | LegifranceCitation | JudilibreCitation | WebCitation;

/** GET /runs/:id/citations — 50 by default, 100 at most, first-use order. */
export interface AgentCitationList {
	items: AgentCitation[];
	total: number;
	/** The `number` to pass as `after`; null on the last page. */
	next_after: number | null;
}

/** GET /citations/:id — the citation plus a bounded passage of the frozen source. */
export type AgentCitationDetail = AgentCitation & {
	source_available: boolean;
	passage: string;
	/** Code-point range of the quote inside `passage`. */
	highlight_start: number;
	highlight_end: number;
};

