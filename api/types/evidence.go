package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Evidence and coverage contracts.
// Evidence source types reuse the CitationSource* vocabulary: piece,
// legifrance, judilibre, web.

// Locator kinds. A DOCX locator never carries a page number: there is no
// physical page to cite.
const (
	LocatorPDF         = "pdf"
	LocatorDOCX        = "docx"
	LocatorXLSX        = "xlsx"
	LocatorLegislation = "legislation"
	LocatorDecision    = "decision"
	LocatorWeb         = "web"
)

// SourceLocator is a tagged locator: Kind says which fields are set.
//
//	pdf          Page, Paragraph (the paragraph of the page holding the span)
//	docx         Paragraph — never a page: a DOCX has none until laid out
//	xlsx         Sheet (the worksheet's real name), Cell ("B4" or "B4:D9")
//	legislation  ArticleID, VersionID, Paragraph
//	decision     DecisionID, Paragraph
//	web          URL
//
// Start/End are half-open Unicode code-point offsets into the kind's text
// unit — the extracted page text (pdf), the paragraph (docx, decision), the
// cell (xlsx), the article version text (legislation), the returned excerpt
// (web) — over the exact extraction the span was read from. For a pièce,
// TextVersion names that text representation (services.PieceTextVersion);
// a locator without one predates it (pages only, no paragraph or cell).
type SourceLocator struct {
	Kind string `json:"kind"` // Locator*
	// SegmentID is the canonical segment of a piece's extraction that
	// coverage counts (pieces only).
	SegmentID  string `json:"segment_id,omitempty"`
	Page       int    `json:"page,omitempty"`      // 1-based
	Paragraph  int    `json:"paragraph,omitempty"` // 1-based
	Sheet      string `json:"sheet,omitempty"`
	Cell       string `json:"cell,omitempty"`
	ArticleID  string `json:"article_id,omitempty"` // official id (LEGIARTI…)
	VersionID  string `json:"version_id,omitempty"` // legal_versions.id
	DecisionID string `json:"decision_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	// TextVersion is the pièce text representation Start/End count in.
	TextVersion string `json:"text_version,omitempty"`
}

// Web source extents: whether only the provider's excerpt was available or
// the full text was retrieved.
const (
	WebExtentExcerpt  = "excerpt"
	WebExtentFullText = "full_text"
)

// EvidenceSpanKey is the content-derived identity of an evidence span: the
// same passage retrieved twice in a run converges on one row
// (agent_evidence_spans.span_key).
func EvidenceSpanKey(sourceType, sourceID, sourceSHA256 string, loc SourceLocator, quote string) string {
	locJSON, _ := json.Marshal(loc) // a struct of plain fields cannot fail
	h := sha256.New()
	for _, part := range []string{sourceType, sourceID, sourceSHA256, string(locJSON), quote} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Coverage (§6.2)
// ---------------------------------------------------------------------------

// Per-piece coverage states.
const (
	CoverageComplete   = "complete"   // every readable segment consumed
	CoveragePartial    = "partial"    // some segments consumed
	CoverageUnread     = "unread"     // in scope, nothing consumed
	CoveragePending    = "pending"    // ingestion not finished
	CoverageUnreadable = "unreadable" // no text could be extracted
	CoverageExcluded   = "excluded"   // removed from scope with provenance
)

// SegmentCounts counts canonical segments. Overlapping reads count once.
type SegmentCounts struct {
	Expected   int `json:"expected"`
	Consumed   int `json:"consumed"`
	Missing    int `json:"missing"`
	Pending    int `json:"pending"`
	Unreadable int `json:"unreadable"`
	Excluded   int `json:"excluded"`
}

// PieceCoverage is one scoped piece's reading state.
type PieceCoverage struct {
	PieceID  string        `json:"piece_id"`
	Ordinal  int           `json:"ordinal"`
	Name     string        `json:"name"`
	State    string        `json:"state"` // Coverage*
	Unit     string        `json:"unit"`  // what a segment is: page | paragraph | cell
	Segments SegmentCounts `json:"segments"`
	// Mutated is true when the piece's original or extraction changed after
	// it was read: its receipts no longer prove anything.
	Mutated bool `json:"mutated"`
}

// CoverageReport is computed by the server from read receipts — never from
// model output. It proves source text was supplied to the agent, not that it
// was understood.
type CoverageReport struct {
	RunID       string `json:"run_id"`
	ScopeSHA256 string `json:"scope_sha256"`
	Exhaustive  bool   `json:"exhaustive"`
	// Complete: every readable scoped segment consumed, no piece pending or
	// unreadable, no source mutated. Only then may exhaustive work be
	// presented as complete.
	Complete    bool            `json:"complete"`
	TotalPieces int             `json:"total_pieces"`
	FullyRead   int             `json:"fully_read"`
	Segments    SegmentCounts   `json:"segments"`
	Pieces      []PieceCoverage `json:"pieces"`
	ComputedAt  int64           `json:"computed_at"`
}
