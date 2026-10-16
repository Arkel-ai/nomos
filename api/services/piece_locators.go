package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var locatorsLog = u.NewLogger("services.piece_locators")

// Exact pièce locators.
//
// A citation points into one precisely versioned text representation of the
// pièce, PieceTextVersion. Its rules:
//
//	pdf   The text of each page as `pdftotext -layout` (or OCR) wrote it: one
//	      segment per page, "p<n>". A paragraph is a run of non-blank lines;
//	      paragraphs tile the page — the first starts at offset 0, each next
//	      one at its first non-blank line, and the blank lines between them
//	      belong to the paragraph before.
//	docx  One segment per non-empty body paragraph (table cells and text boxes
//	      included), "para<n>", numbered in document order. Text is w:t, a tab
//	      is \t, a line break \n, a non-breaking hyphen U+2011; deleted text,
//	      field codes and mc:Fallback copies are not text. There is no page.
//	xlsx  One segment per non-empty cell, "s<k>!<ref>", worksheets in workbook
//	      order under their real names. Text is the string as written (shared
//	      or inline, phonetic runs left out), TRUE/FALSE, an error code, or a
//	      number in its stored decimal form — an ISO 8601 date when its format
//	      is a date or time, the stored form plus " EUR" / " GBP" when its
//	      format shows € or £.
//
// Offsets are half-open Unicode code-point offsets into a segment's text. The
// text sidecar (-text.txt) holds the text; for DOCX and XLSX the extraction
// record (-extract.json) says where each paragraph or cell sits in it, and for
// a scan it keeps the expected page count and the OCR cap, which
// Piece.PageCount forgets once OCR has run.
//
// A piece ingested before this version has no record. A PDF's sidecar already
// is its pdftotext output, so its segments are cut from it directly; a DOCX or
// XLSX is re-read from its retained original with the same parser. Retrieval
// chunks are never treated as the document.

// PieceTextVersion names the text representation every offset is counted in.
// Changing a rule above means a new version: receipts and spans pinned to the
// old one stop counting.
const PieceTextVersion = "nomos-extract/2"

// Extraction limitation codes: what the representation does not hold, said
// rather than hidden. Safe to log.
const (
	LimitOCRPageCap         = "ocr_page_cap"
	LimitDOCXNotes          = "docx_notes_not_read"
	LimitDOCXComments       = "docx_comments_not_read"
	LimitDOCXHeadersFooters = "docx_headers_footers_not_read"
	LimitDOCXListLabels     = "docx_list_labels_not_in_text"
	LimitXLSXHiddenSheet    = "xlsx_hidden_sheet"
	LimitXLSXOtherSheets    = "xlsx_non_worksheet_not_read"
	LimitXLSXSheetMissing   = "xlsx_sheet_missing"
	LimitXLSXFormulaNoValue = "xlsx_formula_without_value"
	LimitXLSXCellLimit      = "xlsx_cell_limit"
)

// Why a segment has no text. Safe to log.
const (
	SegmentNoText     = "no_text"      // a page without extractable text
	SegmentOCRMissing = "ocr_missing"  // OCR wrote nothing for this page
	SegmentOCRCap     = "ocr_page_cap" // past the OCR cap: never processed
	SegmentCellLimit  = "cell_limit"   // workbook cells past maxXLSXCells
)

// ExtractionLimitation is one thing the extraction left out. Detail may name
// a sheet: it is shown to the reader, never logged.
type ExtractionLimitation struct {
	Code   string `json:"code"`
	Count  int    `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// extractionRecord is the -extract.json sidecar.
type extractionRecord struct {
	Version string `json:"version"`
	Format  string `json:"format"`
	// TextSHA256 is the hash of the -text.txt this record indexes; a record
	// whose text changed under it is ignored.
	TextSHA256     string                 `json:"text_sha256,omitempty"`
	Scanned        bool                   `json:"scanned,omitempty"`
	ExpectedPages  int                    `json:"expected_pages,omitempty"`
	ProcessedPages int                    `json:"processed_pages,omitempty"`
	OCRPageCap     int                    `json:"ocr_page_cap,omitempty"`
	Sheets         []string               `json:"sheets,omitempty"`
	Units          []extractionUnit       `json:"units,omitempty"`
	Truncated      bool                   `json:"truncated,omitempty"`
	Limitations    []ExtractionLimitation `json:"limitations,omitempty"`
}

// extractionUnit is one DOCX paragraph or XLSX cell: where its text sits in
// the sidecar (page, half-open code-point offsets) and what it is.
type extractionUnit struct {
	Page      int    `json:"page"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Paragraph int    `json:"paragraph,omitempty"`
	Sheet     int    `json:"sheet,omitempty"` // 1-based, into Sheets
	Cell      string `json:"cell,omitempty"`
}

// extraction is a parser's output: the sidecar pages and their record.
type extraction struct {
	Pages  []ingestPage
	Record extractionRecord
}

// pdfExtractionRecord is a PDF's record: the pages pdftotext found and, for a
// scan, how many of them OCR will process — the cap stays visible here after
// chunking rewrites Piece.PageCount to the pages actually read.
func pdfExtractionRecord(pages []ingestPage, scanned bool, ocrPages int) extractionRecord {
	rec := extractionRecord{Version: PieceTextVersion, Format: pieceFormatPDF,
		ExpectedPages: len(pages), ProcessedPages: len(pages)}
	if scanned {
		rec.Scanned, rec.ProcessedPages, rec.OCRPageCap = true, ocrPages, maxOCRPages
		if ocrPages < len(pages) {
			rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitOCRPageCap, Count: len(pages) - ocrPages})
		}
	}
	return rec
}

// ---------------------------------------------------------------------------
// Segments
// ---------------------------------------------------------------------------

// segmentPart is one paragraph of a PDF page segment: [Start, End) in the
// page text. Parts tile the page.
type segmentPart struct {
	Paragraph  int
	Start, End int
}

// pdfParagraphParts cuts a page into paragraphs (PieceTextVersion rules).
func pdfParagraphParts(text string) []segmentPart {
	r := []rune(text)
	var starts []int
	lineStart, prevBlank := 0, true
	for i := 0; i <= len(r); i++ {
		if i < len(r) && r[i] != '\n' {
			continue
		}
		blank := true
		for _, c := range r[lineStart:i] {
			if !unicode.IsSpace(c) {
				blank = false
				break
			}
		}
		if !blank && prevBlank {
			starts = append(starts, lineStart)
		}
		prevBlank, lineStart = blank, i+1
	}
	parts := make([]segmentPart, len(starts))
	for k := range starts {
		start, end := starts[k], len(r)
		if k == 0 {
			start = 0
		}
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		parts[k] = segmentPart{Paragraph: k + 1, Start: start, End: end}
	}
	return parts
}

// pdfSegments makes one segment per page. missing marks pages whose OCR text
// was never written.
func pdfSegments(pages []ingestPage, missing map[int]bool) []PieceSegment {
	out := make([]PieceSegment, 0, len(pages))
	for _, pg := range pages {
		seg := PieceSegment{ID: "p" + strconv.Itoa(pg.Number), Page: pg.Number, Text: pg.Text, unit: pg.Number}
		switch {
		case missing[pg.Number]:
			seg.Unreadable, seg.Reason = true, SegmentOCRMissing
		case strings.TrimSpace(pg.Text) == "":
			// In a text PDF an image page never OCR'd, in a scan a page OCR
			// could not read: neither is "nothing there".
			seg.Unreadable, seg.Reason = true, SegmentNoText
		default:
			seg.Parts = pdfParagraphParts(pg.Text)
		}
		out = append(out, seg)
	}
	return out
}

// errRecordMismatch means a record does not fit the text it claims to index.
var errRecordMismatch = errors.New("extraction record does not match its text")

// officeSegments cuts the DOCX paragraphs or XLSX cells a record indexes out
// of the sidecar pages.
func officeSegments(format string, pages []ingestPage, rec extractionRecord) ([]PieceSegment, error) {
	texts := make(map[int][]rune, len(pages))
	for _, pg := range pages {
		texts[pg.Number] = []rune(pg.Text)
	}
	out := make([]PieceSegment, 0, len(rec.Units)+1)
	for _, un := range rec.Units {
		page, ok := texts[un.Page]
		if !ok || un.Start < 0 || un.Start > un.End || un.End > len(page) {
			return nil, errRecordMismatch
		}
		seg := PieceSegment{Text: string(page[un.Start:un.End]), unit: un.Page, base: un.Start}
		switch format {
		case pieceFormatDOCX:
			seg.ID, seg.Paragraph = "para"+strconv.Itoa(un.Paragraph), un.Paragraph
		default:
			if un.Sheet < 1 || un.Sheet > len(rec.Sheets) || un.Cell == "" {
				return nil, errRecordMismatch
			}
			seg.ID = fmt.Sprintf("s%d!%s", un.Sheet, un.Cell)
			seg.Sheet, seg.SheetIndex, seg.Cell = rec.Sheets[un.Sheet-1], un.Sheet, un.Cell
		}
		out = append(out, seg)
	}
	if rec.Truncated {
		// The cells past the bound exist but were not extracted: one
		// unreadable segment keeps the piece from ever reading as complete.
		out = append(out, PieceSegment{ID: "cells-beyond-limit", Unreadable: true, Reason: SegmentCellLimit})
	}
	return out, nil
}

// applyRecord copies what the reader is told about the extraction.
func (t *PieceText) applyRecord(rec *extractionRecord) {
	if rec == nil {
		return
	}
	t.ExpectedPages, t.ProcessedPages, t.OCRPageCap = rec.ExpectedPages, rec.ProcessedPages, rec.OCRPageCap
	t.Sheets = rec.Sheets
	t.Limitations = rec.Limitations
}

// passageBounds are the citable passages of a segment: its paragraphs on a
// PDF page, the whole segment otherwise.
func (s *PieceSegment) passageBounds() []segmentPart {
	if len(s.Parts) > 0 {
		return s.Parts
	}
	return []segmentPart{{Paragraph: s.Paragraph, Start: 0, End: s.Runes}}
}

// piecePassage is one citable range: a whole or partial paragraph of a PDF
// page, of a DOCX paragraph, or of an XLSX cell.
type piecePassage struct {
	Seg        *PieceSegment
	Start, End int
	Paragraph  int
	Text       string
}

// passagesOf cuts one delivered range at its paragraph boundaries. The
// passages tile the range exactly.
func (t *PieceText) passagesOf(r segmentRange) []piecePassage {
	var out []piecePassage
	for _, p := range r.Seg.passageBounds() {
		start, end := max(p.Start, r.Start), min(p.End, r.End)
		if start >= end {
			continue
		}
		out = append(out, piecePassage{Seg: r.Seg, Start: start, End: end, Paragraph: p.Paragraph,
			Text: string(r.Seg.runeSlice()[start:end])})
	}
	return out
}

// passageOverheadRunes is what one delivered passage costs in an observation
// beyond its text (its evidence id, twice, and its locator fields). The
// reading budget counts it, so a window of many short paragraphs or cells
// stays inside the observation bound instead of being evicted whole.
const passageOverheadRunes = 120

func (t *PieceText) passageOverhead() int {
	switch t.Format {
	case pieceFormatPDF, pieceFormatDOCX, pieceFormatXLSX, pieceFormatOther:
		return passageOverheadRunes
	}
	return 0 // public texts read through the same window
}

// ---------------------------------------------------------------------------
// Locators
// ---------------------------------------------------------------------------

// pieceLocator is the citable locator of one passage.
func pieceLocator(t PieceText, s *PieceSegment, start, end, paragraph int) types.SourceLocator {
	loc := types.SourceLocator{SegmentID: s.ID, Start: start, End: end, TextVersion: t.Version}
	switch t.Format {
	case pieceFormatDOCX:
		loc.Kind, loc.Paragraph = types.LocatorDOCX, s.Paragraph
	case pieceFormatXLSX:
		loc.Kind, loc.Sheet, loc.Cell = types.LocatorXLSX, s.Sheet, s.Cell
	default:
		loc.Kind, loc.Page, loc.Paragraph = types.LocatorPDF, s.Page, paragraph
	}
	return loc
}

// segmentLocator is the locator a search hit or a cursor points at: the
// segment and, on a PDF page, the paragraph holding the offset.
func (t *PieceText) segmentLocator(s *PieceSegment, off int) types.SourceLocator {
	paragraph := s.Paragraph
	for _, p := range s.Parts {
		if off >= p.Start && off < p.End {
			paragraph = p.Paragraph
			break
		}
	}
	return pieceLocator(*t, s, off, off, paragraph)
}

var reBareSheetName = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_.]*$`)

// excelRef is a cell in spreadsheet notation: Récap!B4, 'Salaire de
// référence'!B8. A sheet without a cell is the sheet's name alone.
func excelRef(sheet, cell string) string {
	switch {
	case cell == "":
		return sheet
	case sheet == "":
		return cell
	}
	if !reBareSheetName.MatchString(sheet) {
		sheet = "'" + strings.ReplaceAll(sheet, "'", "''") + "'"
	}
	return sheet + "!" + cell
}

// pieceLocatorText is a pièce locator in French prose: « p. 2, § 3 »,
// « § 4 », « 'Chiffrage des demandes'!D8 ». A DOCX or XLSX locator never
// reads as a page. Empty when the locator names no place.
func pieceLocatorText(loc types.SourceLocator) string {
	switch loc.Kind {
	case types.LocatorPDF:
		switch {
		case loc.Page > 0 && loc.Paragraph > 0:
			return fmt.Sprintf("p. %d, § %d", loc.Page, loc.Paragraph)
		case loc.Page > 0:
			return fmt.Sprintf("p. %d", loc.Page)
		}
	case types.LocatorDOCX:
		if loc.Paragraph > 0 {
			return fmt.Sprintf("§ %d", loc.Paragraph)
		}
	case types.LocatorXLSX:
		if loc.Cell == "" && loc.Sheet != "" {
			return "feuille « " + loc.Sheet + " »"
		}
		return excelRef(loc.Sheet, loc.Cell)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Retrieval chunks → canonical offsets
// ---------------------------------------------------------------------------

// segmentAt maps an offset in a sidecar page onto the segment holding it
// (or, between segments, the next one) and the offset inside it.
func (t *PieceText) segmentAt(unit, offset int) (*PieceSegment, int, bool) {
	var next *PieceSegment
	for i := range t.Segments {
		s := &t.Segments[i]
		if s.unit != unit || s.Unreadable {
			continue
		}
		if offset >= s.base && offset < s.base+s.Runes {
			return s, offset - s.base, true
		}
		if s.base >= offset && (next == nil || s.base < next.base) {
			next = s
		}
	}
	if next != nil {
		return next, 0, true
	}
	return nil, 0, false
}

// locateChunk maps a retrieval chunk onto the canonical text: the segment
// and code-point offset where the chunk's text begins.
//
// The chunker is deterministic and every chunk segment carries its source
// position, so re-cutting the same extraction reproduces the chunk with its
// exact anchor — and a chunk that opens on carried overlap points where that
// text actually is, not where the chunk was cut. A chunk cut from an earlier
// extraction is found by content instead: its first paragraph under the
// quote normalisation, mapped back to original offsets.
func (t *PieceText) locateChunk(chunk string, pageFrom int) (*PieceSegment, int, bool) {
	if t.State != PieceTextReady || len(t.units) == 0 {
		return nil, 0, false
	}
	for _, ch := range chunkPages(t.units, chunkTargetTokens, chunkOverlapTokens) {
		if ch.Text == chunk && (pageFrom <= 0 || ch.PageFrom == pageFrom) {
			return t.segmentAt(ch.StartPage, ch.StartOffset)
		}
	}
	first := chunk
	for _, para := range strings.Split(chunk, "\n\n") {
		if strings.TrimSpace(para) != "" {
			first = para
			break
		}
	}
	needle, _ := normalizeWithOffsets(first)
	if len(needle) < 12 {
		return nil, 0, false
	}
	// A page-based hit is looked for from its first page on, then anywhere.
	units := make([]ingestPage, 0, len(t.units))
	for _, pg := range t.units {
		if pg.Number >= pageFrom {
			units = append(units, pg)
		}
	}
	for _, pg := range t.units {
		if pg.Number < pageFrom {
			units = append(units, pg)
		}
	}
	for _, pg := range units {
		hay, orig := normalizeWithOffsets(pg.Text)
		hayText := string(hay)
		idx := strings.Index(hayText, string(needle))
		if idx < 0 {
			continue
		}
		return t.segmentAt(pg.Number, orig[utf8.RuneCountInString(hayText[:idx])])
	}
	return nil, 0, false
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// loadExtractionRecord reads a piece's record; nil when there is none or it
// is of another version.
func loadExtractionRecord(ctx context.Context, repo *u.TenantRepo, piece types.Piece) (*extractionRecord, error) {
	raw, found, err := blobText(ctx, extractRecordKey(repo.TenantID(), piece.DossierID, piece.ID))
	if err != nil || !found {
		return nil, err
	}
	var rec extractionRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Version != PieceTextVersion {
		locatorsLog.Warnf("Extraction record unusable, derived again (piece=%s)", piece.ID)
		return nil, nil
	}
	return &rec, nil
}

// legacyCacheSize bounds the re-read office originals kept in memory.
const legacyCacheSize = 32

var legacyCache = struct {
	sync.Mutex
	entries map[string]extraction
	order   []string
}{entries: map[string]extraction{}}

// legacyOfficeExtraction re-reads a DOCX or XLSX original that has no usable
// record — ingested before PieceTextVersion — with today's parser. A reason
// says why the piece cannot be read (original gone, unparsable); an error is
// a store failure.
func legacyOfficeExtraction(ctx context.Context, asset *types.Asset, format string) (extraction, string, error) {
	key := asset.SHA256 + "|" + format + "|" + PieceTextVersion
	legacyCache.Lock()
	ex, ok := legacyCache.entries[key]
	legacyCache.Unlock()
	if ok {
		return ex, "", nil
	}
	raw, err := ingestFetch(ctx, asset.S3Key)
	if err != nil {
		var se *runner.StepError
		if errors.As(err, &se) && !se.Retryable {
			return extraction{}, "original_missing", nil
		}
		return extraction{}, "", err
	}
	if format == pieceFormatDOCX {
		ex, err = docxExtract(raw)
	} else {
		ex, err = xlsxExtract(raw)
	}
	if err != nil {
		var se *runner.StepError
		if errors.As(err, &se) && !se.Retryable {
			return extraction{}, "parse_failed", nil
		}
		return extraction{}, "", err
	}
	legacyCache.Lock()
	if _, ok := legacyCache.entries[key]; !ok {
		legacyCache.entries[key] = ex
		legacyCache.order = append(legacyCache.order, key)
		if len(legacyCache.order) > legacyCacheSize {
			delete(legacyCache.entries, legacyCache.order[0])
			legacyCache.order = legacyCache.order[1:]
		}
	}
	legacyCache.Unlock()
	locatorsLog.Infof("Legacy office piece re-read from its original (asset=%s format=%s units=%d)", asset.ID, format, len(ex.Record.Units))
	return ex, "", nil
}

// scanPageCounts says how many pages a scan has and how many OCR processed:
// from its record, else (ingested before records) from the extract_text
// step's output, else from Piece.PageCount.
func scanPageCounts(repo *u.TenantRepo, piece types.Piece, rec *extractionRecord) (expected, processed, ocrCap int) {
	if rec != nil && rec.Scanned {
		return rec.ExpectedPages, rec.ProcessedPages, rec.OCRPageCap
	}
	var step types.RunStep
	err := repo.RunSteps().Select("output").
		Where("step_key = ? AND state = ?", StepKindExtractText+":"+piece.ID, types.StepStateSucceeded).
		Order("created_at DESC").Take(&step).Error
	var out extractOutput
	if err == nil && jsonUnmarshalStepOutput(step.Output, &out) == nil && out.Scanned && out.OCRPages > 0 {
		return out.Pages, out.OCRPages, maxOCRPages
	}
	return piece.PageCount, piece.PageCount, 0
}
