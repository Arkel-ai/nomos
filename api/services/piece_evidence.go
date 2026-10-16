package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var evidenceLog = u.NewLogger("services.piece_evidence")

// Exact private evidence and coverage.
//
// A pièce is read through its CANONICAL EXTRACTION, never through the
// overlapping retrieval chunks: the text sidecar extract_text wrote (pages
// separated by form feeds) indexed by its extraction record or, for a scan,
// the per-page OCR texts. That text is cut into segments in the unit a lawyer
// cites (PieceTextVersion, piece_locators.go):
//
//	pdf   one segment per page        p<n>        locator page, paragraph + offsets in the page
//	docx  one segment per paragraph   para<n>     locator paragraph + offsets in it — no page
//	xlsx  one segment per cell        s<k>!<ref>  locator sheet name, cell + offsets in it
//
// Offsets are half-open Unicode code-point offsets into the segment's exact
// text. ManifestSHA256 hashes every segment: a receipt or a span pinned to a
// manifest hash proves nothing once the piece's text has changed.
//
// The coverage ledger counts a segment as read only from receipts the runtime
// marked consumed (consumed_dispatch set): the text appeared in an accepted
// controller input. Fetching it, summarising it or listing the piece's
// metadata counts for nothing, and overlapping reads count once. A scan's
// pages past the OCR cap are segments too — unreadable, never silently
// absent.

// Piece text states.
const (
	PieceTextReady      = "ready"
	PieceTextPending    = "pending"
	PieceTextUnreadable = "unreadable"
)

// Segment units, as reported on types.PieceCoverage.Unit.
const (
	SegmentUnitPage      = "page"
	SegmentUnitParagraph = "paragraph"
	SegmentUnitCell      = "cell"
)

// Document formats.
const (
	pieceFormatPDF   = "pdf"
	pieceFormatDOCX  = "docx"
	pieceFormatXLSX  = "xlsx"
	pieceFormatOther = "other"
)

// maxScopePieces bounds one turn's documentary scope. Beyond it a tool says
// so (processing_limit) instead of silently reading the first rows.
const maxScopePieces = 2000

// ocrFetchConcurrency bounds the parallel page fetches of one scan.
const ocrFetchConcurrency = 8

// ErrMissingDossier means the turn has no dossier: private tools answer
// missing_dossier.
var ErrMissingDossier = errors.New("the turn has no dossier")

// ErrScopeTooLarge means the dossier exceeds maxScopePieces.
var ErrScopeTooLarge = errors.New("the dossier exceeds the agent's piece limit")

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

// PieceScopeSnapshot is the turn's frozen documentary scope, resolved against
// the current rows: every scoped piece whatever its ingestion state, the
// pieces excluded with provenance, and explicit inclusions that no longer
// exist.
type PieceScopeSnapshot struct {
	DossierID  string
	Mode       string
	Pieces     []types.Piece // in scope, ordinal order
	Excluded   []types.Piece
	MissingIDs []string
	Assets     map[string]types.Asset // by asset id
	byID       map[string]int
}

// Contains reports whether a piece id is in scope.
func (s PieceScopeSnapshot) Contains(pieceID string) bool {
	_, ok := s.byID[pieceID]
	return ok
}

// Piece returns a scoped piece.
func (s PieceScopeSnapshot) Piece(pieceID string) (types.Piece, bool) {
	i, ok := s.byID[pieceID]
	if !ok {
		return types.Piece{}, false
	}
	return s.Pieces[i], true
}

// IDs lists the scoped piece ids in ordinal order.
func (s PieceScopeSnapshot) IDs() []string {
	out := make([]string, 0, len(s.Pieces))
	for _, p := range s.Pieces {
		out = append(out, p.ID)
	}
	return out
}

// SnapshotPieceScope resolves a frozen scope. A ranked search never redefines
// it: it is the whole dossier minus explicit exclusions, or exactly the
// explicit inclusions — an explicit empty list is an empty scope.
//
// ErrMissingDossier is the no-dossier case; a dossier that no longer exists in
// the tenant is an error the run fails on.
func SnapshotPieceScope(ctx context.Context, repo *u.TenantRepo, scope types.AgentSourceScope) (PieceScopeSnapshot, error) {
	snap := PieceScopeSnapshot{DossierID: scope.DossierID, Mode: scope.Mode, Assets: map[string]types.Asset{}, byID: map[string]int{}}
	if scope.Mode == types.ScopeNone || scope.Mode == "" || scope.DossierID == "" {
		return snap, ErrMissingDossier
	}
	var dossiers int64
	if err := repo.Dossiers().Where("id = ?", scope.DossierID).Count(&dossiers).Error; err != nil {
		return snap, err
	}
	if dossiers == 0 {
		return snap, fmt.Errorf("dossier %s is no longer reachable in this tenant", scope.DossierID)
	}

	var total int64
	if err := repo.Pieces().Where("dossier_id = ?", scope.DossierID).Count(&total).Error; err != nil {
		return snap, err
	}
	if total > maxScopePieces {
		return snap, ErrScopeTooLarge
	}
	var all []types.Piece
	if err := repo.Pieces().Where("dossier_id = ?", scope.DossierID).
		Order("ordinal ASC, created_at ASC, id ASC").Find(&all).Error; err != nil {
		return snap, err
	}

	excluded := map[string]bool{}
	included := map[string]bool{}
	switch scope.Mode {
	case types.ScopeAllPieces:
		for _, e := range scope.Excluded {
			excluded[e.PieceID] = true
		}
	case types.ScopeExplicit:
		for _, e := range scope.Included {
			included[e.PieceID] = true
		}
	default:
		return snap, fmt.Errorf("unknown scope mode %q", scope.Mode)
	}

	seen := map[string]bool{}
	assetIDs := make([]string, 0, len(all))
	for _, p := range all {
		seen[p.ID] = true
		inScope := scope.Mode == types.ScopeAllPieces && !excluded[p.ID] ||
			scope.Mode == types.ScopeExplicit && included[p.ID]
		if inScope {
			snap.byID[p.ID] = len(snap.Pieces)
			snap.Pieces = append(snap.Pieces, p)
			assetIDs = append(assetIDs, p.SourceAssetID)
		} else if excluded[p.ID] {
			snap.Excluded = append(snap.Excluded, p)
			assetIDs = append(assetIDs, p.SourceAssetID)
		}
	}
	for _, e := range scope.Included {
		if !seen[e.PieceID] {
			snap.MissingIDs = append(snap.MissingIDs, e.PieceID)
		}
	}
	if len(assetIDs) > 0 {
		var assets []types.Asset
		if err := repo.Assets().Where("id IN ?", assetIDs).Find(&assets).Error; err != nil {
			return snap, err
		}
		for _, a := range assets {
			snap.Assets[a.ID] = a
		}
	}
	return snap, nil
}

// pieceName is how a piece is named to the agent and in activity rows.
func pieceName(p types.Piece) string {
	return firstNonEmpty(strings.TrimSpace(p.FinalName), strings.TrimSpace(p.ProposedName),
		strings.TrimSpace(p.Title), strings.TrimSpace(p.OriginalName))
}

// pieceLabel is "Pièce n° 3 — name".
func pieceLabel(p types.Piece) string {
	return truncateRunes(fmt.Sprintf("Pièce n° %d — %s", p.Ordinal, pieceName(p)), 140)
}

// ---------------------------------------------------------------------------
// Canonical text
// ---------------------------------------------------------------------------

// PieceSegment is one canonical segment of a piece's extraction.
type PieceSegment struct {
	ID        string
	Index     int
	Page      int
	Paragraph int
	// Sheet is an XLSX cell's worksheet name, SheetIndex its 1-based place
	// in the workbook, Cell its reference ("B4").
	Sheet      string
	SheetIndex int
	Cell       string
	Text       string
	Runes      int
	// Parts are a PDF page's paragraphs, tiling the page.
	Parts []segmentPart
	// Unreadable marks a page with no extractable text — an image page in a
	// text PDF, a page OCR could not read or never reached. It is reported,
	// never "read". Reason says which (Segment*).
	Unreadable bool
	Reason     string
	// unit and base place the text in the sidecar page it was cut from, so a
	// retrieval chunk's source offset maps back onto it.
	unit, base int
	runes      []rune
}

func (s *PieceSegment) runeSlice() []rune {
	if s.runes == nil {
		s.runes = []rune(s.Text)
	}
	return s.runes
}

// PieceText is a piece's canonical extraction, segmented.
type PieceText struct {
	PieceID string
	Format  string
	Unit    string
	State   string // PieceText*
	// Reason says why the text is pending or unreadable (a code, safe to log).
	Reason         string
	SourceSHA256   string // the original's hash
	ManifestSHA256 string // the extraction's hash, over every segment
	// Version is the text representation offsets are counted in.
	Version  string
	Segments []PieceSegment
	// What the reader is told about the extraction: a PDF's expected and
	// processed pages and OCR cap, a workbook's sheets, what was left out.
	ExpectedPages  int
	ProcessedPages int
	OCRPageCap     int
	Sheets         []string
	Limitations    []ExtractionLimitation
	// units are the sidecar pages the segments were cut from — what the
	// chunker cut retrieval chunks from.
	units []ingestPage
	index map[string]int
}

// Readable counts the segments that carry text.
func (t PieceText) Readable() int {
	n := 0
	for _, s := range t.Segments {
		if !s.Unreadable {
			n++
		}
	}
	return n
}

// UnreadableIDs lists the segments without extractable text.
func (t PieceText) UnreadableIDs() []string {
	var out []string
	for _, s := range t.Segments {
		if s.Unreadable {
			out = append(out, s.ID)
		}
	}
	return out
}

func (t *PieceText) segment(id string) (*PieceSegment, bool) {
	i, ok := t.index[id]
	if !ok {
		return nil, false
	}
	return &t.Segments[i], true
}

func pieceFormat(mime string) string {
	switch mime {
	case "application/pdf":
		return pieceFormatPDF
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return pieceFormatDOCX
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return pieceFormatXLSX
	}
	return pieceFormatOther
}

func unitFor(format string) string {
	switch format {
	case pieceFormatDOCX:
		return SegmentUnitParagraph
	case pieceFormatXLSX:
		return SegmentUnitCell
	}
	return SegmentUnitPage
}

// LoadPieceText reads a piece's canonical extraction. A piece whose
// ingestion has not finished is pending; one that produced no text is
// unreadable. Neither is an error: an error means the store or the database
// failed.
func LoadPieceText(ctx context.Context, repo *u.TenantRepo, piece types.Piece, asset *types.Asset) (PieceText, error) {
	if asset == nil {
		var a types.Asset
		if err := repo.Assets().Where("id = ?", piece.SourceAssetID).First(&a).Error; err != nil {
			return PieceText{}, fmt.Errorf("original of piece %s: %w", piece.ID, err)
		}
		asset = &a
	}
	t := PieceText{
		PieceID:      piece.ID,
		Format:       pieceFormat(asset.Mime),
		SourceSHA256: asset.SHA256,
		Version:      PieceTextVersion,
	}
	t.Unit = unitFor(t.Format)

	sidecar, found, err := blobText(ctx, textSidecarKey(repo.TenantID(), piece.DossierID, piece.ID))
	if err != nil {
		return t, err
	}
	rec, err := loadExtractionRecord(ctx, repo, piece)
	if err != nil {
		return t, err
	}
	if found {
		if rec != nil && rec.TextSHA256 != sha256Hex(sidecar) {
			rec = nil // the text changed under its record
		}
		pages := splitFormFeeds(sidecar)
		switch t.Format {
		case pieceFormatDOCX, pieceFormatXLSX:
			var segs []PieceSegment
			if rec != nil {
				segs, err = officeSegments(t.Format, pages, *rec)
			}
			if rec == nil || err != nil {
				// No usable record: ingested before PieceTextVersion. The
				// original is re-read with the same parser; the old sidecar's
				// flattened text is not a citable document.
				ex, reason, err := legacyOfficeExtraction(ctx, asset, t.Format)
				if err != nil {
					return t, err
				}
				if reason != "" {
					t.State, t.Reason = PieceTextUnreadable, reason
					return t, nil
				}
				if segs, err = officeSegments(t.Format, ex.Pages, ex.Record); err != nil {
					return t, err
				}
				pages, rec = ex.Pages, &ex.Record
			}
			t.Segments = segs
		default:
			t.Segments = pdfSegments(pages, nil)
			if rec == nil {
				rec = &extractionRecord{ExpectedPages: len(pages), ProcessedPages: len(pages)}
			}
		}
		t.State, t.units = PieceTextReady, pages
		t.applyRecord(rec)
		return t.finish(), nil
	}

	// No sidecar: a scan read by OCR, or ingestion still running.
	extract, err := latestStepState(repo, StepKindExtractText+":"+piece.ID)
	if err != nil {
		return t, err
	}
	if t.Format == pieceFormatPDF {
		chunk, err := latestStepState(repo, StepKindChunkAndEmbed+":"+piece.ID)
		if err != nil {
			return t, err
		}
		switch {
		case chunk == types.StepStateSucceeded, chunk == types.StepStateFailed:
			// The OCR fan-out is over: whatever pages it wrote are the
			// text, a page it could not write is reported unreadable, and so
			// is every page past the OCR cap — counted from the record, not
			// from Piece.PageCount, which chunking rewrote to the pages read.
			expected, processed, ocrCap := scanPageCounts(repo, piece, rec)
			pages, missing, err := ocrPages(ctx, repo, piece, processed)
			if err != nil {
				return t, err
			}
			t.Segments = pdfSegments(pages, missing)
			for n := processed + 1; n <= expected; n++ {
				t.Segments = append(t.Segments, PieceSegment{ID: "p" + strconv.Itoa(n), Page: n, unit: n,
					Unreadable: true, Reason: SegmentOCRCap})
			}
			t.State, t.units = PieceTextReady, pages
			t.applyRecord(&extractionRecord{ExpectedPages: max(expected, processed), ProcessedPages: processed, OCRPageCap: ocrCap})
			if rec != nil {
				t.Limitations = rec.Limitations
			} else if expected > processed {
				t.Limitations = []ExtractionLimitation{{Code: LimitOCRPageCap, Count: expected - processed}}
			}
			return t.finish(), nil
		case stepActive(chunk), stepActive(extract),
			chunk == "" && extract == "" && piece.ClassificationState == types.PieceStatePending:
			t.State, t.Reason = PieceTextPending, "ingestion_running"
			return t, nil
		}
		t.State, t.Reason = PieceTextUnreadable, "no_text_extracted"
		return t, nil
	}
	if stepActive(extract) || extract == "" && piece.ClassificationState == types.PieceStatePending {
		t.State, t.Reason = PieceTextPending, "ingestion_running"
		return t, nil
	}
	t.State, t.Reason = PieceTextUnreadable, "no_text_extracted"
	return t, nil
}

// finish indexes the segments, hashes the manifest and settles the state.
func (t PieceText) finish() PieceText {
	t.index = make(map[string]int, len(t.Segments))
	h := sha256.New()
	h.Write([]byte(t.Version))
	h.Write([]byte{0})
	h.Write([]byte(t.Format))
	for i := range t.Segments {
		s := &t.Segments[i]
		s.Index = i
		s.Runes = len(s.runeSlice())
		t.index[s.ID] = i
		// A renamed sheet changes every cell's locator: it is part of the
		// identity, like the text.
		for _, part := range []string{s.ID, s.Sheet, s.Text} {
			h.Write([]byte{0})
			h.Write([]byte(part))
		}
	}
	t.ManifestSHA256 = hex.EncodeToString(h.Sum(nil))
	if t.Readable() == 0 {
		t.State, t.Reason = PieceTextUnreadable, "no_text_extracted"
	}
	return t
}

// ocrPages fetches the first n page texts of a scan. A page whose text object
// is missing stays in the list, empty, and is marked missing so it is
// reported unreadable rather than dropped.
func ocrPages(ctx context.Context, repo *u.TenantRepo, piece types.Piece, n int) ([]ingestPage, map[int]bool, error) {
	pages := make([]ingestPage, n)
	found := make([]bool, n)
	errs := make([]error, n)
	sem := make(chan struct{}, ocrFetchConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			text, ok, err := blobText(ctx, pageTextKey(repo.TenantID(), piece.DossierID, piece.ID, i+1))
			pages[i] = ingestPage{Number: i + 1, Text: text}
			found[i], errs[i] = ok, err
		}(i)
	}
	wg.Wait()
	missing := map[int]bool{}
	for i, err := range errs {
		if err != nil {
			return nil, nil, err
		}
		if !found[i] {
			missing[i+1] = true
		}
	}
	return pages, missing, nil
}

// blobText reads a derived text object. A missing object is not an error:
// it comes back as found=false.
func blobText(ctx context.Context, key string) (string, bool, error) {
	rc, err := u.Blob.Get(ctx, key)
	if err != nil {
		if errors.Is(err, u.ErrBlobNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxIngestFileBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(raw) > maxIngestFileBytes {
		return "", false, fmt.Errorf("derived text object exceeds %d bytes", maxIngestFileBytes)
	}
	return string(raw), true, nil
}

// latestStepState is the state of the most recent step with this key across
// the tenant's runs ("" when none exists). Ingestion keys are piece-unique.
func latestStepState(repo *u.TenantRepo, key string) (string, error) {
	var step types.RunStep
	err := repo.RunSteps().Select("state").Where("step_key = ?", key).
		Order("created_at DESC").Take(&step).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return step.State, nil
}

func stepActive(state string) bool {
	switch state {
	case types.StepStateQueued, types.StepStateRunning, types.StepStateWaitingInput:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Reading windows
// ---------------------------------------------------------------------------

// segmentRange is one exact range delivered from one segment.
type segmentRange struct {
	Seg   *PieceSegment
	Start int
	End   int
	Text  string
}

// minWindowTail is the smallest remainder worth cutting a segment for; below
// it the window stops at the segment boundary instead.
const minWindowTail = 400

// FormatSegmentCursor is the cursor read_piece accepts: segment@offset.
func FormatSegmentCursor(segmentID string, offset int) string {
	return segmentID + "@" + strconv.Itoa(offset)
}

// parseSegmentCursor reads "p3@120", "para12" or "" (the start).
func (t *PieceText) parseSegmentCursor(cursor string) (int, int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, 0, nil
	}
	id, offText, hasOffset := strings.Cut(cursor, "@")
	seg, ok := t.segment(id)
	if !ok {
		return 0, 0, badArgs("curseur inconnu : le segment %q n'existe pas dans ce document", id)
	}
	off := 0
	if hasOffset {
		n, err := strconv.Atoi(offText)
		if err != nil || n < 0 || n > seg.Runes {
			return 0, 0, badArgs("curseur invalide : décalage hors du segment %s", id)
		}
		off = n
	}
	return seg.Index, off, nil
}

// readWindow delivers exact ranges from the cursor until budget code points
// are used — each delivered passage (a PDF paragraph, a DOCX paragraph, an
// XLSX cell) also costing its observation overhead. A segment too long for
// what remains stops at a paragraph boundary or is cut at a line or word
// boundary, and the next cursor resumes exactly there. Unreadable segments on
// the way are listed, never delivered as text.
func (t *PieceText) readWindow(cursor string, budget int) (ranges []segmentRange, skipped []string, next string, err error) {
	i, off, err := t.parseSegmentCursor(cursor)
	if err != nil {
		return nil, nil, "", err
	}
	remaining := budget
	over := t.passageOverhead()
	for ; i < len(t.Segments); i, off = i+1, 0 {
		s := &t.Segments[i]
		if s.Unreadable {
			skipped = append(skipped, s.ID)
			continue
		}
		if s.Runes-off <= 0 {
			continue
		}
		end, cost, stop := s.fit(off, remaining, over, len(ranges) > 0)
		if end > off {
			ranges = append(ranges, segmentRange{Seg: s, Start: off, End: end, Text: string(s.runeSlice()[off:end])})
			remaining -= cost
		}
		if stop {
			return ranges, skipped, FormatSegmentCursor(s.ID, end), nil
		}
	}
	return ranges, skipped, "", nil
}

// fit says how far a window may read into a segment from off: whole
// passages while they fit, then — unless what remains is too small to be
// worth a cut after something was already delivered — a cut inside the next
// one. stop is true when the window ends inside this segment.
func (s *PieceSegment) fit(off, remaining, over int, delivered bool) (end, cost int, stop bool) {
	end = off
	for _, p := range s.passageBounds() {
		if p.End <= off {
			continue
		}
		from := max(p.Start, off)
		if c := p.End - from + over; cost+c <= remaining {
			cost += c
			end = p.End
			continue
		}
		room := remaining - cost - over
		if (delivered || end > off) && room < minWindowTail {
			return end, cost, true
		}
		room = max(room, 1)
		cut := cutBoundary(s.runeSlice(), from, from+room)
		return cut, cost + cut - from + over, true
	}
	return end, cost, false
}

// cutBoundary picks a cut in (start, limit]: the last line break, else the
// last space, in the second half of the window; else the limit itself.
func cutBoundary(r []rune, start, limit int) int {
	if limit >= len(r) {
		return len(r)
	}
	floor := start + (limit-start)/2
	for i := limit; i > floor; i-- {
		if r[i-1] == '\n' {
			return i
		}
	}
	for i := limit; i > floor; i-- {
		if unicode.IsSpace(r[i-1]) {
			return i
		}
	}
	return limit
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

func init() { PieceManifestFor = pieceManifest }

// pieceManifest is the verifier's view of a piece's canonical extraction:
// the same segments read_piece returns, versioned the way its receipts are
// (the original's hash and the extraction manifest hash).
func pieceManifest(ctx context.Context, repo *u.TenantRepo, piece types.Piece) (PieceManifest, error) {
	text, err := LoadPieceText(ctx, repo, piece, nil)
	if err != nil {
		return PieceManifest{}, err
	}
	m := PieceManifest{SourceSHA256: text.SourceSHA256, ExtractionVersion: text.ManifestSHA256, Unit: text.Unit}
	switch text.State {
	case PieceTextPending:
		m.State = types.CoveragePending
		return m, nil
	case PieceTextUnreadable:
		m.State = types.CoverageUnreadable
		return m, nil
	}
	for _, seg := range text.Segments {
		m.Segments = append(m.Segments, ManifestSegment{ID: seg.ID, Runes: seg.Runes, Readable: !seg.Unreadable})
	}
	return m, nil
}

// ComputePieceCoverage is the server's coverage report for a run: computed
// from consumed read receipts against the current canonical extraction of
// every scoped piece. It is what the final gate checks an exhaustive request
// against, and it proves the text was supplied — not that it was understood.
func ComputePieceCoverage(ctx context.Context, repo *u.TenantRepo, runID string, contract types.AgentTaskContract) (types.CoverageReport, error) {
	report := types.CoverageReport{RunID: runID, Exhaustive: contract.Exhaustive, Pieces: []types.PieceCoverage{}, ComputedAt: time.Now().Unix()}
	snap, err := SnapshotPieceScope(ctx, repo, contract.Scope)
	if errors.Is(err, ErrMissingDossier) {
		// No dossier, no private scope: nothing to read, nothing missing.
		report.Complete = true
		report.ScopeSHA256 = sha256Hex("scope:none")
		return report, nil
	}
	if err != nil {
		return report, err
	}

	var receipts []types.AgentReadReceipt
	if err := repo.AgentReadReceipts().
		Where("run_id = ? AND consumed_dispatch IS NOT NULL", runID).
		Find(&receipts).Error; err != nil {
		return report, err
	}
	byPiece := map[string][]types.AgentReadReceipt{}
	for _, r := range receipts {
		byPiece[r.PieceID] = append(byPiece[r.PieceID], r)
	}

	scopeHash := sha256.New()
	fmt.Fprintf(scopeHash, "mode=%s dossier=%s\n", snap.Mode, snap.DossierID)
	complete := true
	for _, p := range snap.Pieces {
		var asset *types.Asset
		if a, ok := snap.Assets[p.SourceAssetID]; ok {
			asset = &a
		}
		text, err := LoadPieceText(ctx, repo, p, asset)
		if err != nil {
			return report, err
		}
		pc := pieceCoverage(p, text, byPiece[p.ID])
		fmt.Fprintf(scopeHash, "piece=%s source=%s manifest=%s state=%s\n", p.ID, text.SourceSHA256, text.ManifestSHA256, text.State)
		report.Pieces = append(report.Pieces, pc)
		addCounts(&report.Segments, pc.Segments)
		if pc.State == types.CoverageComplete {
			report.FullyRead++
		} else {
			complete = false
		}
	}
	for _, p := range snap.Excluded {
		pc := types.PieceCoverage{PieceID: p.ID, Ordinal: p.Ordinal, Name: pieceName(p), State: types.CoverageExcluded,
			Unit: unitFor(pieceFormat(snap.Assets[p.SourceAssetID].Mime)), Segments: types.SegmentCounts{Excluded: max(p.PageCount, 1)}}
		fmt.Fprintf(scopeHash, "excluded=%s\n", p.ID)
		report.Pieces = append(report.Pieces, pc)
		addCounts(&report.Segments, pc.Segments)
	}
	for _, id := range snap.MissingIDs {
		// An explicit inclusion that no longer exists cannot be read: the
		// scope the lawyer set is not fully available.
		fmt.Fprintf(scopeHash, "missing=%s\n", id)
		report.Pieces = append(report.Pieces, types.PieceCoverage{PieceID: id, State: types.CoverageUnreadable,
			Segments: types.SegmentCounts{Unreadable: 1}})
		report.Segments.Unreadable++
		complete = false
	}
	report.TotalPieces = len(snap.Pieces) + len(snap.MissingIDs)
	report.Complete = complete
	report.ScopeSHA256 = hex.EncodeToString(scopeHash.Sum(nil))
	evidenceLog.Debugf("Coverage computed (run=%s pieces=%d fully_read=%d expected=%d consumed=%d complete=%t)",
		runID, report.TotalPieces, report.FullyRead, report.Segments.Expected, report.Segments.Consumed, report.Complete)
	return report, nil
}

func addCounts(dst *types.SegmentCounts, c types.SegmentCounts) {
	dst.Expected += c.Expected
	dst.Consumed += c.Consumed
	dst.Missing += c.Missing
	dst.Pending += c.Pending
	dst.Unreadable += c.Unreadable
	dst.Excluded += c.Excluded
}

// pieceCoverage counts one piece. A segment is consumed when the union of
// its consumed receipt ranges covers it entirely; receipts cut from another
// version of the text count for nothing and flag the piece as mutated.
func pieceCoverage(p types.Piece, text PieceText, receipts []types.AgentReadReceipt) types.PieceCoverage {
	pc := types.PieceCoverage{PieceID: p.ID, Ordinal: p.Ordinal, Name: pieceName(p), Unit: text.Unit}
	switch text.State {
	case PieceTextPending:
		pc.State = types.CoveragePending
		pc.Segments.Pending = max(p.PageCount, 1)
		return pc
	case PieceTextUnreadable:
		pc.State = types.CoverageUnreadable
		pc.Segments.Unreadable = max(len(text.Segments), 1)
		return pc
	}

	ranges := map[string][][2]int{}
	read := false
	for _, r := range receipts {
		if r.ExtractionVersion != text.ManifestSHA256 || r.SourceSHA256 != text.SourceSHA256 {
			pc.Mutated = true
			continue
		}
		ranges[r.SegmentID] = append(ranges[r.SegmentID], [2]int{r.StartOffset, r.EndOffset})
		read = true
	}
	for i := range text.Segments {
		s := &text.Segments[i]
		if s.Unreadable {
			pc.Segments.Unreadable++
			continue
		}
		pc.Segments.Expected++
		if covers(ranges[s.ID], s.Runes) {
			pc.Segments.Consumed++
		} else {
			pc.Segments.Missing++
		}
	}
	switch {
	case pc.Segments.Expected > 0 && pc.Segments.Consumed == pc.Segments.Expected && !pc.Mutated:
		pc.State = types.CoverageComplete
	case read:
		// Something was read, even if no segment is whole yet.
		pc.State = types.CoveragePartial
	default:
		pc.State = types.CoverageUnread
	}
	return pc
}

// covers reports whether the union of half-open ranges spans [0, n).
func covers(ranges [][2]int, n int) bool {
	if n == 0 {
		return true
	}
	if len(ranges) == 0 {
		return false
	}
	sorted := append([][2]int(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })
	reach := 0
	for _, r := range sorted {
		if r[0] > reach {
			return false
		}
		if r[1] > reach {
			reach = r[1]
		}
		if reach >= n {
			return true
		}
	}
	return reach >= n
}

// consumedSegments counts, per piece, the distinct segments that already have
// a consumed receipt in this run — a cheap progress hint for list_pieces,
// never a coverage claim.
func consumedSegments(repo *u.TenantRepo, runID string, pieceIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(pieceIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		PieceID string
		N       int
	}
	if err := repo.AgentReadReceipts().
		Select("piece_id, COUNT(DISTINCT segment_id) AS n").
		Where("run_id = ? AND consumed_dispatch IS NOT NULL AND piece_id IN ?", runID, pieceIDs).
		Group("piece_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PieceID] = r.N
	}
	return out, nil
}
