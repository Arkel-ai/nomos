package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The coverage ledger: canonical segments, exact
// reading windows, and coverage computed only from consumed receipts.

// ---------------------------------------------------------------------------
// Pure
// ---------------------------------------------------------------------------

func TestSegmentsFollowTheCitedUnit(t *testing.T) {
	pdf := PieceText{Format: pieceFormatPDF, State: PieceTextReady, Version: PieceTextVersion,
		Segments: pdfSegments(splitFormFeeds("Page un.\f   \fPage trois.\f"), nil)}.finish()
	if len(pdf.Segments) != 3 || pdf.Segments[1].ID != "p2" || !pdf.Segments[1].Unreadable || pdf.Readable() != 2 {
		t.Fatalf("pdf: one segment per page, an empty page unreadable: %+v", pdf.Segments)
	}

	ex, err := docxExtract(testDOCX("Premier.", "  ", "Deuxième\tparagraphe.", "Troisième."))
	if err != nil {
		t.Fatal(err)
	}
	segs, err := officeSegments(pieceFormatDOCX, ex.Pages, ex.Record)
	if err != nil {
		t.Fatal(err)
	}
	docx := PieceText{Format: pieceFormatDOCX, State: PieceTextReady, Version: PieceTextVersion, Segments: segs}.finish()
	if len(docx.Segments) != 3 || docx.Segments[1].ID != "para2" || docx.Segments[1].Paragraph != 2 ||
		docx.Segments[1].Text != "Deuxième\tparagraphe." || docx.Segments[1].Page != 0 {
		t.Fatalf("docx: one segment per non-empty paragraph, no page: %+v", docx.Segments)
	}

	empty := PieceText{Format: pieceFormatPDF, State: PieceTextReady, Segments: pdfSegments([]ingestPage{{Number: 1, Text: " "}}, nil)}.finish()
	if empty.State != PieceTextUnreadable {
		t.Fatal("a piece without any text is unreadable")
	}
	if pdf.ManifestSHA256 == "" || pdf.ManifestSHA256 == docx.ManifestSHA256 {
		t.Fatal("manifest hash must identify the extraction")
	}
}

func TestReadWindowDeliversExactContiguousRanges(t *testing.T) {
	long := strings.Repeat("mot ", 600) // 2400 runes
	text := PieceText{Format: pieceFormatPDF, State: PieceTextReady, Segments: []PieceSegment{
		{ID: "p1", Page: 1, Text: "Page un, accentuée : é à ç."},
		{ID: "p2", Page: 2, Text: "", Unreadable: true},
		{ID: "p3", Page: 3, Text: long},
	}}.finish()

	var all []segmentRange
	cursor, calls := "", 0
	var skipped []string
	for {
		ranges, sk, next, err := text.readWindow(cursor, 1000)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ranges...)
		skipped = append(skipped, sk...)
		calls++
		if next == "" {
			break
		}
		cursor = next
		if calls > 10 {
			t.Fatal("window does not progress")
		}
	}
	if calls != 3 || len(skipped) != 1 || skipped[0] != "p2" {
		t.Fatalf("calls=%d skipped=%v", calls, skipped)
	}
	// Every readable code point is delivered exactly once, in order.
	var p3 strings.Builder
	reach := 0
	for _, r := range all {
		if r.Seg.ID != "p3" {
			if r.Start != 0 || r.End != r.Seg.Runes || r.Text != r.Seg.Text {
				t.Fatalf("p1 must be delivered whole and exact: %+v", r)
			}
			continue
		}
		if r.Start != reach {
			t.Fatalf("gap or overlap at %d (start %d)", reach, r.Start)
		}
		if got := string([]rune(long)[r.Start:r.End]); got != r.Text {
			t.Fatal("range text must be the exact code-point slice")
		}
		reach = r.End
		p3.WriteString(r.Text)
	}
	if reach != len([]rune(long)) || p3.String() != long {
		t.Fatalf("p3 delivered up to %d of %d", reach, len([]rune(long)))
	}
	// Cuts land on a boundary (after a space), never mid-word.
	if !strings.HasSuffix(all[1].Text, " ") {
		t.Fatalf("cut not on a word boundary: %q", all[1].Text[len(all[1].Text)-5:])
	}

	for _, bad := range []string{"p9", "p3@-1", "p3@99999", "p3@x"} {
		if _, _, _, err := text.readWindow(bad, 1000); err == nil {
			t.Fatalf("cursor %q must be refused", bad)
		}
	}
	if ranges, _, next, _ := text.readWindow("p3@2400", 1000); len(ranges) != 0 || next != "" {
		t.Fatal("a cursor at the end delivers nothing and ends")
	}
}

func TestCoversCountsTheUnionOnce(t *testing.T) {
	cases := []struct {
		ranges [][2]int
		n      int
		want   bool
	}{
		{nil, 0, true},
		{nil, 10, false},
		{[][2]int{{0, 10}}, 10, true},
		{[][2]int{{5, 10}, {0, 6}}, 10, true},
		{[][2]int{{0, 4}, {5, 10}}, 10, false},
		{[][2]int{{0, 4}, {0, 4}, {4, 9}}, 10, false},
		{[][2]int{{0, 7}, {2, 3}, {6, 10}}, 10, true},
	}
	for i, c := range cases {
		if got := covers(c.ranges, c.n); got != c.want {
			t.Fatalf("case %d: covers=%t want %t", i, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Database: read_piece receipts and coverage math on a seeded dossier
// ---------------------------------------------------------------------------

func consume(t *testing.T, f *toolFixture, callIDs ...string) {
	t.Helper()
	if err := u.DB.Exec("UPDATE agent_read_receipts SET consumed_dispatch = 1 WHERE tenant_id = ? AND run_id = ? AND tool_call_id IN ?",
		f.tenant, f.runID, callIDs).Error; err != nil {
		t.Fatal(err)
	}
}

func coverageOf(t *testing.T, f *toolFixture, c types.AgentTaskContract) (types.CoverageReport, map[string]types.PieceCoverage) {
	t.Helper()
	r, err := ComputePieceCoverage(context.Background(), f.repo(), f.runID, c)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	by := map[string]types.PieceCoverage{}
	for _, p := range r.Pieces {
		by[p.PieceID] = p
	}
	return r, by
}

func TestReadPieceReceiptsAndCoverageMath(t *testing.T) {
	f := newToolFixture(t)
	page1 := strings.Repeat("Le salarié a été convoqué à un entretien préalable. ", 40) // ~2080 runes
	a := f.addPiece(pieceSpec{ordinal: 1, name: "convocation.pdf", mime: mimePDF, pages: []string{page1, "", "Signature."}})
	b := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 2, "attestation.docx",
		testDOCX("Je soussigné atteste.", "", "Le 12 mars 2025.", "Fait pour servir."), false)
	pending := f.addPiece(pieceSpec{ordinal: 3, name: "scan.pdf", mime: mimePDF, state: types.PieceStatePending, pageCnt: 2})
	excluded := f.addPiece(pieceSpec{ordinal: 4, name: "brouillon.pdf", mime: mimePDF, pages: []string{"Brouillon."}})

	c := f.contract()
	c.Scope.Excluded = []types.ScopeEntry{{PieceID: excluded.ID, Provenance: types.ContractProvenance{Source: types.ContractFromLawyerAnswer, QuestionKey: "q1"}}}

	// Piece A in two windows: page 1 is cut, then finished with page 3; the
	// empty page 2 is reported, not read.
	x1 := f.exec(c)
	first := f.call(x1, types.ToolReadPiece, map[string]any{"piece_id": a.ID, "cursor": nil, "max_chars": 1000})
	wantOK(t, first)
	next := first.Observation.NextCursor
	if !first.Observation.Truncated || !strings.HasPrefix(next, "p1@") {
		t.Fatalf("first window must stop inside page 1: %q", next)
	}
	x2 := f.exec(c)
	second := f.call(x2, types.ToolReadPiece, map[string]any{"piece_id": a.ID, "cursor": next, "max_chars": 24000})
	wantOK(t, second)
	d2 := obsData(t, second)
	if d2["end_of_piece"] != true || len(d2["skipped_unreadable"].([]any)) != 1 {
		t.Fatalf("second window must finish the piece and report p2: %v", d2)
	}

	var receipts []types.AgentReadReceipt
	f.repo().AgentReadReceipts().Where("run_id = ? AND piece_id = ?", f.runID, a.ID).Order("segment_id, start_offset").Find(&receipts)
	if len(receipts) != 3 {
		t.Fatalf("want 3 receipts (p1 twice, p3), got %d", len(receipts))
	}
	if receipts[0].SegmentID != "p1" || receipts[0].StartOffset != 0 || receipts[1].StartOffset != receipts[0].EndOffset ||
		receipts[1].EndOffset != len([]rune(page1)) || receipts[0].ConsumedDispatch != nil {
		t.Fatalf("receipts must be contiguous exact ranges, unconsumed: %+v", receipts)
	}
	var spans []types.AgentEvidenceSpan
	f.repo().AgentEvidenceSpans().Where("run_id = ? AND piece_id = ?", f.runID, a.ID).Find(&spans)
	if len(spans) != 3 {
		t.Fatalf("one evidence span per delivered range, got %d", len(spans))
	}
	var loc types.SourceLocator
	_ = json.Unmarshal(spans[0].Locator, &loc)
	if loc.Kind != types.LocatorPDF || loc.Page == 0 || loc.SegmentID == "" {
		t.Fatalf("pdf locator: %+v", loc)
	}

	// Nothing consumed yet: delivered is not read.
	report, by := coverageOf(t, f, c)
	if report.Complete || by[a.ID].State != types.CoverageUnread || by[a.ID].Segments.Expected != 2 || by[a.ID].Segments.Unreadable != 1 {
		t.Fatalf("unconsumed receipts must not count: %+v", by[a.ID])
	}
	if by[pending.ID].State != types.CoveragePending || by[excluded.ID].State != types.CoverageExcluded {
		t.Fatalf("pending/excluded states: %+v %+v", by[pending.ID], by[excluded.ID])
	}

	// Only the first window consumed: partial.
	consume(t, f, x1.CallID)
	_, by = coverageOf(t, f, c)
	if by[a.ID].State != types.CoveragePartial || by[a.ID].Segments.Consumed != 0 {
		t.Fatalf("half a page is not a page: %+v", by[a.ID])
	}
	consume(t, f, x2.CallID)
	_, by = coverageOf(t, f, c)
	if by[a.ID].State != types.CoverageComplete || by[a.ID].Segments.Consumed != 2 {
		t.Fatalf("piece A must be complete: %+v", by[a.ID])
	}

	// Overlapping re-read counts once.
	x3 := f.exec(c)
	f.call(x3, types.ToolReadPiece, map[string]any{"piece_id": a.ID, "cursor": "p1@100", "max_chars": 1000})
	consume(t, f, x3.CallID)
	_, by = coverageOf(t, f, c)
	if by[a.ID].Segments.Consumed != 2 || by[a.ID].Segments.Expected != 2 {
		t.Fatalf("overlap inflated coverage: %+v", by[a.ID])
	}

	// Piece B, a DOCX: paragraph segments.
	x4 := f.exec(c)
	readB := f.call(x4, types.ToolReadPiece, map[string]any{"piece_id": b.ID, "cursor": nil, "max_chars": nil})
	if obsData(t, readB)["unit"] != SegmentUnitParagraph || len(readB.Observation.ReadReceiptIDs) != 3 {
		t.Fatalf("docx read: %s", readB.Observation.Data)
	}
	consume(t, f, x4.CallID)
	report, by = coverageOf(t, f, c)
	if by[b.ID].State != types.CoverageComplete || report.FullyRead != 2 || report.TotalPieces != 3 {
		t.Fatalf("two of three scoped pieces fully read: %+v", report)
	}
	if report.Complete {
		t.Fatal("a pending piece forbids a complete exhaustive claim")
	}
	scopeBefore := report.ScopeSHA256

	// The extraction of A changes: its receipts prove nothing any more.
	f.blob.objects[textSidecarKey(f.tenant, f.dossier.ID, a.ID)] = []byte(page1 + " Ajout.\f\fSignature.")
	report, by = coverageOf(t, f, c)
	if !by[a.ID].Mutated || by[a.ID].State != types.CoverageUnread || by[a.ID].Segments.Consumed != 0 {
		t.Fatalf("mutated source must void its receipts: %+v", by[a.ID])
	}
	if report.ScopeSHA256 == scopeBefore {
		t.Fatal("a text change must change the scope hash")
	}

	// Pending piece: read_piece says so, retryably.
	out := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": pending.ID, "cursor": nil, "max_chars": nil})
	wantCode(t, out, types.ToolCodeSourceUnavailable)
	if !out.Observation.Retryable {
		t.Fatal("a pending piece is worth retrying later")
	}
}

func TestScannedPieceReadsOCRPagesAndReportsMissingOnes(t *testing.T) {
	f := newToolFixture(t)
	scan := f.addPiece(pieceSpec{ordinal: 1, name: "scan.pdf", mime: mimePDF, pageCnt: 3})
	run := types.RunStep{ID: uuid.NewString(), RunID: "ingest_" + uuid.NewString(), StepKey: StepKindChunkAndEmbed + ":" + scan.ID,
		Kind: types.StepKindLLM, State: types.StepStateRunning, AvailableAt: time.Now(), IdempotencyKey: uuid.NewString()}
	if err := f.repo().Create(&run); err != nil {
		t.Fatal(err)
	}
	c := f.contract()
	out := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": scan.ID, "cursor": nil, "max_chars": nil})
	wantCode(t, out, types.ToolCodeSourceUnavailable) // OCR still running

	u.DB.Model(&types.RunStep{}).Where("id = ?", run.ID).Update("state", types.StepStateSucceeded)
	f.blob.objects[pageTextKey(f.tenant, f.dossier.ID, scan.ID, 1)] = []byte("Page lue par OCR.")
	f.blob.objects[pageTextKey(f.tenant, f.dossier.ID, scan.ID, 3)] = []byte("Dernière page.")
	x := f.exec(c)
	out = f.call(x, types.ToolReadPiece, map[string]any{"piece_id": scan.ID, "cursor": nil, "max_chars": nil})
	wantOK(t, out)
	d := obsData(t, out)
	if len(d["unreadable_segments"].([]any)) != 1 || d["unreadable_segments"].([]any)[0] != "p2" {
		t.Fatalf("the page OCR never wrote is unreadable: %v", d)
	}
	consume(t, f, x.CallID)
	_, by := coverageOf(t, f, c)
	if by[scan.ID].State != types.CoverageComplete || by[scan.ID].Segments.Unreadable != 1 {
		t.Fatalf("readable pages consumed, the missing one reported: %+v", by[scan.ID])
	}
}
