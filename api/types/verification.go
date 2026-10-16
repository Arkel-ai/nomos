package types

// Verification and citation contracts. One
// verifier serves both the agent's verify_output tool and the final gate, and
// the model never supplies a trusted result. Pièce mismatches warn and keep
// download available; legal and integrity failures block.

// Issue severities.
const (
	SeverityWarning  = "warning"
	SeverityBlocking = "blocking"
)

// Report results. `warnings` is publishable with download allowed, but is
// never presented as a green verified state.
const (
	VerificationPassed   = "passed"
	VerificationWarnings = "warnings"
	VerificationBlocked  = "blocked"
)

// The checks, one per verification concern (§7.1).
const (
	CheckOwnership        = "ownership" // tenant, dossier, scope, revision, evidence
	CheckStructure        = "structure"
	CheckCompetence       = "competence" // required sections and clauses
	CheckPieceClaims      = "piece_claims"
	CheckLegalCitations   = "legal_citations"
	CheckWebProvenance    = "web_provenance"
	CheckCoverage         = "coverage"
	CheckLawyerDecisions  = "lawyer_decisions"
	CheckReview           = "review" // a review cycle seen by a later decision (§7.4)
	CheckArtifactFidelity = "artifact_fidelity"
)

// Check statuses.
const (
	CheckPassed        = "passed"
	CheckWarning       = "warning"
	CheckBlocked       = "blocked"
	CheckNotApplicable = "not_applicable"
)

// Issue codes. Pièce codes are warnings; the others block unless the
// verifier documents otherwise.
const (
	// Integrity.
	IssueEvidenceUnauthorized = "evidence_unauthorized"
	IssueEvidenceUnknown      = "evidence_unknown"
	IssueSourceMutated        = "source_mutated"
	IssueRevisionHashMismatch = "revision_hash_mismatch"
	IssueStructureInvalid     = "structure_invalid"

	// Pièces — warn, never block download.
	IssuePieceQuoteNotFound       = "piece_quote_not_found"
	IssuePieceDateNotFound        = "piece_date_not_found"
	IssuePieceAmountMismatch      = "piece_amount_mismatch"
	IssuePieceCalculationMissing  = "piece_calculation_unsupported"
	IssuePieceMetadataOnlySupport = "piece_metadata_only"

	// Legal citations — resolve or refuse.
	IssueLegalNotRetrieved      = "legal_not_retrieved"
	IssueLegalUnresolved        = "legal_unresolved"
	IssueLegalQuoteNotFound     = "legal_quote_not_found"
	IssueLegalVersionNotInForce = "legal_version_not_applicable"
	IssueCaselawAfterCutoff     = "caselaw_after_cutoff"
	IssueLegalReferenceUncited  = "legal_reference_uncited" // a reference in prose without a citation
	IssueLegalOfficialMismatch  = "legal_official_mismatch"
	IssueLegalOfficialDown      = "legal_official_unavailable" // retryable

	// Web.
	IssueWebExcerptMissing = "web_excerpt_missing" // a URL without excerpt is a lead, not support
	IssueWebURLNotReturned = "web_url_not_returned"

	// Scope, contract and delivery.
	IssueCoverageIncomplete    = "coverage_incomplete"
	IssueCoveragePending       = "coverage_pending"
	IssueCompetenceSection     = "competence_section_missing"
	IssueCompetenceClause      = "competence_clause_missing"
	IssueLawyerDecisionOpen    = "lawyer_decision_unresolved"
	IssueReviewMissing         = "review_missing"
	IssueArtifactFidelity      = "artifact_fidelity_mismatch"
	IssueArtifactWarningAbsent = "artifact_warning_missing"
)

// VerificationIssue names one problem precisely. Correction is server-written
// guidance for the agent; it never quotes source text.
type VerificationIssue struct {
	Code        string   `json:"code"`     // Issue*
	Severity    string   `json:"severity"` // Severity*
	Check       string   `json:"check"`    // Check*
	BlockID     string   `json:"block_id,omitempty"`
	ClaimID     string   `json:"claim_id,omitempty"`
	CitationID  string   `json:"citation_id,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Correction  string   `json:"correction,omitempty"`
	Retryable   bool     `json:"retryable"`
}

// VerificationCheck is one check's outcome.
type VerificationCheck struct {
	Check   string `json:"check"`   // Check*
	Status  string `json:"status"`  // CheckPassed…
	Checked int    `json:"checked"` // items examined: claims, citations, segments
}

// VerificationReport is the server-created result for one document revision
// (the answer is a revision of kind `answer`). Publication loads it and
// rechecks every hash; a report whose hashes do not match is no report.
type VerificationReport struct {
	VerifierVersion    string `json:"verifier_version"`
	DocumentRevisionID string `json:"document_revision_id"`
	ContentSHA256      string `json:"content_sha256"`
	// ScopeSHA256 covers the frozen scope and its source hashes;
	// EvidenceSHA256 the evidence spans and consumed read receipts checked.
	ScopeSHA256     string              `json:"scope_sha256"`
	EvidenceSHA256  string              `json:"evidence_sha256"`
	Result          string              `json:"result"` // Verification*
	DownloadAllowed bool                `json:"download_allowed"`
	Warnings        int                 `json:"warnings"`
	Blocking        int                 `json:"blocking"`
	Checks          []VerificationCheck `json:"checks"`
	Issues          []VerificationIssue `json:"issues"`
	Coverage        *CoverageReport     `json:"coverage,omitempty"`
	// Official lists the legal evidence the final gate revalidated against
	// the official source (Légifrance getArticle, Judilibre decision): only
	// successful round-trips; a failure is an issue. A legal citation absent
	// from it was never officially verified.
	Official  []OfficialCheck `json:"official,omitempty"`
	CheckedAt int64           `json:"checked_at"`
}

// OfficialCheck is one legal evidence span whose exact stored version the
// official source returned under the same identity, with the same text and
// an applicability compatible with the one requested.
type OfficialCheck struct {
	EvidenceID     string `json:"evidence_id"`
	LegalVersionID string `json:"legal_version_id"`
	// OfficialID is the identity the official source answered with: the
	// LEGIARTI version id, or the Judilibre decision id.
	OfficialID string `json:"official_id"`
	// TextSHA256 hashes the official text, whitespace-normalised.
	TextSHA256 string `json:"text_sha256"`
	CheckedAt  int64  `json:"checked_at"`
}

// ---------------------------------------------------------------------------
// Citations (§7.5)
// ---------------------------------------------------------------------------

// Per-citation checks. Each is a separate result; none implies another.
const (
	CitationCheckSourceResolved  = "source_resolved"
	CitationCheckQuoteMatched    = "quote_matched"
	CitationCheckTemporalValid   = "temporal_valid"
	CitationCheckOfficialMatched = "official_matched"
	CitationCheckSemanticSupport = "semantic_support"
)

// Per-citation check statuses.
const (
	CitationCheckPassed      = "passed"
	CitationCheckFailed      = "failed"
	CitationCheckUnavailable = "unavailable"
	CitationCheckNotChecked  = "not_checked"
)

// CitationCheck is one check's result on one citation.
type CitationCheck struct {
	Check     string `json:"check"`
	Status    string `json:"status"`
	CheckedAt *int64 `json:"checked_at,omitempty"`
}

// AgentCitation is the wire shape of one verified citation binding, by
// source_type (CitationSourcePiece | Legifrance | Judilibre | Web). Only the
// fields of its type are set; missing source metadata stays missing.
type AgentCitation struct {
	ID string `json:"id"`
	// Number is the server-assigned order of first use in the answer.
	Number             int             `json:"number"`
	SourceType         string          `json:"source_type"`
	ClaimIDs           []string        `json:"claim_ids"`
	MessageID          *string         `json:"message_id"`
	DocumentRevisionID *string         `json:"document_revision_id"`
	EvidenceSpanID     string          `json:"evidence_span_id"`
	Label              string          `json:"label"`
	Quote              string          `json:"quote"`
	Locator            SourceLocator   `json:"locator"`
	SourceSHA256       string          `json:"source_sha256"`
	RetrievedAt        int64           `json:"retrieved_at"`
	CheckedAt          *int64          `json:"checked_at"`
	Checks             []CitationCheck `json:"checks"`
	WarningCodes       []string        `json:"warning_codes"`
	// Human verification, only ever written by a lawyer's action.
	VerifiedBy string `json:"verified_by,omitempty"`
	VerifiedAt *int64 `json:"verified_at,omitempty"`

	// piece
	PieceID      string `json:"piece_id,omitempty"`
	PieceOrdinal int    `json:"piece_ordinal,omitempty"`
	Filename     string `json:"filename,omitempty"`

	// legifrance
	ArticleID         string `json:"article_id,omitempty"`
	ArticleLabel      string `json:"article_label,omitempty"`
	StoredVersionID   string `json:"stored_version_id,omitempty"`
	OfficialVersionID string `json:"official_version_id,omitempty"`
	ValidFrom         *int64 `json:"valid_from,omitempty"`
	ValidTo           *int64 `json:"valid_to,omitempty"`
	ApplicabilityDate *int64 `json:"applicability_date,omitempty"`

	// judilibre
	DecisionID   string `json:"decision_id,omitempty"`
	Court        string `json:"court,omitempty"`
	Chamber      string `json:"chamber,omitempty"`
	DecisionDate *int64 `json:"decision_date,omitempty"`
	ECLI         string `json:"ecli,omitempty"`
	Pourvoi      string `json:"pourvoi,omitempty"`

	// legifrance, judilibre, web
	URL string `json:"url,omitempty"`

	// web — no legal-authority badge
	Host   string `json:"host,omitempty"`
	Title  string `json:"title,omitempty"`
	Extent string `json:"extent,omitempty"` // WebExtent*
}

// AgentCitationList is GET /api/runs/:id/citations: 50 by default, 100 at
// most, in first-use order. NextAfter is the Number to pass as `after`.
type AgentCitationList struct {
	Items     []AgentCitation `json:"items"`
	Total     int64           `json:"total"`
	NextAfter *int            `json:"next_after"`
}

// AgentCitationDetail is GET /api/citations/:id: the citation plus a bounded
// passage of the frozen source, with the quote's code-point range in it.
type AgentCitationDetail struct {
	AgentCitation
	SourceAvailable bool   `json:"source_available"`
	Passage         string `json:"passage"`
	HighlightStart  int    `json:"highlight_start"`
	HighlightEnd    int    `json:"highlight_end"`
}
