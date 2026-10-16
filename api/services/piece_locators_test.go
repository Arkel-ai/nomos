package services

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Exact pièce locators on the fictional
// Vasseur fixtures: DOCX paragraph identities, XLSX sheet names and cells,
// PDF pages and paragraphs, offsets that are exact and survive chunk
// overlap, legacy pieces, the OCR cap, and a pièce mismatch reported at the
// paragraph or cell it concerns.

// ---------------------------------------------------------------------------
// Builders
// ---------------------------------------------------------------------------

const vasseurDir = "../../testdata/dossier-vasseur"

// vasseurFixture reads one fictional pièce; testdata/ is untracked, so a
// checkout without it skips.
func vasseurFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vasseurDir, name))
	if errors.Is(err, os.ErrNotExist) {
		testmode.Missing(t, "fixture %s not present (testdata/ is untracked)", name)
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// testDOCX is a minimal DOCX: one paragraph per argument, a tab as w:tab
// (with a tab stop in the paragraph properties, which is not text).
func testDOCX(paras ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paras {
		b.WriteString(`<w:p><w:pPr><w:tabs><w:tab w:val="left" w:pos="720"/></w:tabs></w:pPr><w:r>`)
		for i, part := range strings.Split(p, "\t") {
			if i > 0 {
				b.WriteString(`<w:tab/>`)
			}
			b.WriteString(`<w:t xml:space="preserve">` + xmlText(part) + `</w:t>`)
		}
		b.WriteString(`</w:r></w:p>`)
	}
	b.WriteString(`</w:body></w:document>`)
	return testZip(map[string][]byte{"word/document.xml": []byte(b.String())})
}

// officeText builds a piece's canonical text from an extraction, the way the
// loader does.
func officeText(t *testing.T, format string, ex extraction) PieceText {
	t.Helper()
	segs, err := officeSegments(format, ex.Pages, ex.Record)
	if err != nil {
		t.Fatal(err)
	}
	pt := PieceText{Format: format, Unit: unitFor(format), State: PieceTextReady, Version: PieceTextVersion, Segments: segs, units: ex.Pages}
	pt.applyRecord(&ex.Record)
	return pt.finish()
}

// addOriginalPiece stores raw as a pièce's original under its real hash and
// extracts it as extract_text does. legacy keeps only what was written before
// PieceTextVersion: a flattened text sidecar, no record.
func addOriginalPiece(t *testing.T, repo *u.TenantRepo, blob *toolBlob, dossierID string, ordinal int, name string, raw []byte, legacy bool) types.Piece {
	t.Helper()
	_, mime, reason := sniffDocument(raw)
	if reason != "" {
		t.Fatalf("%s: %s", name, reason)
	}
	asset := types.Asset{ID: uuid.NewString(), S3Key: u.DossierKey(repo.TenantID(), dossierID, uuid.NewString()+"-"+name),
		Kind: types.AssetKindOriginal, Mime: mime, Size: int64(len(raw)), SHA256: sha256Hex(string(raw))}
	if err := repo.Create(&asset); err != nil {
		t.Fatal(err)
	}
	p := types.Piece{ID: uuid.NewString(), DossierID: dossierID, SourceAssetID: asset.ID, OriginalName: name, Title: name,
		Ordinal: ordinal, ClassificationState: types.PieceStateProposed, DatePrecision: types.DatePrecisionUnknown}
	if err := repo.Create(&p); err != nil {
		t.Fatal(err)
	}
	blob.objects[asset.S3Key] = raw
	if legacy {
		var ex extraction
		var err error
		if mime == mimeDOCX {
			ex, err = docxExtract(raw)
		} else {
			ex, err = xlsxExtract(raw)
		}
		if err != nil {
			t.Fatal(err)
		}
		// Slice 1 wrote a DOCX one line per paragraph and a workbook
		// flattened: neither says where a paragraph or a cell is.
		flat := strings.ReplaceAll(ingestJoinPages(ex.Pages), "\n\n", "\n")
		blob.objects[textSidecarKey(repo.TenantID(), dossierID, p.ID)] = []byte(flat)
		return p
	}
	out, err := extractPiece(context.Background(), repo, p, mime, raw)
	if err != nil {
		t.Fatalf("%s: extract: %v", name, err)
	}
	p.PageCount = out.Pages
	if err := repo.Pieces().Where("id = ?", p.ID).Update("page_count", out.Pages).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func spansOf(t *testing.T, repo *u.TenantRepo, runID, pieceID string) map[string]types.AgentEvidenceSpan {
	t.Helper()
	var spans []types.AgentEvidenceSpan
	if err := repo.AgentEvidenceSpans().Where("run_id = ? AND piece_id = ?", runID, pieceID).Find(&spans).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]types.AgentEvidenceSpan{}
	for _, s := range spans {
		out[s.ID] = s
	}
	return out
}

func locatorOf(t *testing.T, s types.AgentEvidenceSpan) types.SourceLocator {
	t.Helper()
	var loc types.SourceLocator
	if err := json.Unmarshal(s.Locator, &loc); err != nil {
		t.Fatal(err)
	}
	return loc
}

// ---------------------------------------------------------------------------
// Pure: the representation
// ---------------------------------------------------------------------------

func TestDOCXFixturesHaveParagraphIdentities(t *testing.T) {
	cases := []struct {
		file       string
		paragraphs int
		at         int
		contains   string
	}{
		{"06-transcription-sms-2025-03-17.docx", 16, 5, "« Fais partir le 4512 tout de suite."},
		{"10-compte-rendu-entretien-prealable-2025-03-31.docx", 21, 4, "lundi 31 mars 2025, de 10 h 00 à 10 h 52"},
		{"15-attestation-temoin-lemoine.docx", 14, 11, "Fait à Dury, le 12 janvier 2026."},
		{"16-attestation-temoin-arbogast.docx", 13, 7, "jeudi 20 mars 2025, vers 9 h 40"},
	}
	for _, c := range cases {
		ex, err := docxExtract(vasseurFixture(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		text := officeText(t, pieceFormatDOCX, ex)
		if len(text.Segments) != c.paragraphs || text.Unit != SegmentUnitParagraph {
			t.Fatalf("%s: %d paragraphs (%s), want %d", c.file, len(text.Segments), text.Unit, c.paragraphs)
		}
		page := []rune(ex.Pages[0].Text)
		for i, s := range text.Segments {
			if s.ID != "para"+strconv.Itoa(i+1) || s.Paragraph != i+1 || s.Page != 0 {
				t.Fatalf("%s: paragraph %d identity %+v — a DOCX has paragraphs, never a page", c.file, i+1, s)
			}
			if string(page[s.base:s.base+s.Runes]) != s.Text {
				t.Fatalf("%s: §%d is not at its recorded offsets", c.file, s.Paragraph)
			}
		}
		if got := text.Segments[c.at-1].Text; !strings.Contains(got, c.contains) {
			t.Fatalf("%s: §%d = %q, want it to contain %q", c.file, c.at, got, c.contains)
		}
		loc := pieceLocator(text, &text.Segments[c.at-1], 0, 4, 0)
		if loc.Kind != types.LocatorDOCX || loc.Paragraph != c.at || loc.Page != 0 || loc.TextVersion != PieceTextVersion {
			t.Fatalf("%s: docx locator %+v", c.file, loc)
		}
	}
	// The compte rendu's attendees are an automatic list: their bullets are
	// not text, and the record says so.
	ex, _ := docxExtract(vasseurFixture(t, "10-compte-rendu-entretien-prealable-2025-03-31.docx"))
	if len(ex.Record.Limitations) != 1 || ex.Record.Limitations[0].Code != LimitDOCXListLabels {
		t.Fatalf("list labels limitation expected: %+v", ex.Record.Limitations)
	}
}

func TestXLSXFixturesHaveSheetNamesAndCells(t *testing.T) {
	cases := []struct {
		file   string
		sheets []string
		cells  map[string]string
	}{
		{"17-recapitulatif-bulletins-de-paie.xlsx", []string{"Bulletins avr.2024-mars 2025", "Salaire de référence"}, map[string]string{
			"s1!A16": "Mars 2025", "s1!E16": "-974.75 EUR", "s1!F17": "34305.25 EUR", "s2!B8": "2940 EUR",
			"s2!C5": "34 305,25 € / 12", "s2!A10": "SALAIRE DE RÉFÉRENCE RETENU",
		}},
		{"18-note-de-chiffrage-demandes.xlsx", []string{"Chiffrage des demandes", "Barème et hypothèses"}, map[string]string{
			"s1!B8": "Indemnité compensatrice de préavis", "s1!D8": "5880 EUR", "s1!D7": "158.4 EUR",
			"s1!B14": "TOTAL DES DEMANDES", "s1!D14": "38822.87 EUR", "s1!A6": "1",
		}},
	}
	for _, c := range cases {
		ex, err := xlsxExtract(vasseurFixture(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		text := officeText(t, pieceFormatXLSX, ex)
		if strings.Join(text.Sheets, "|") != strings.Join(c.sheets, "|") || text.Unit != SegmentUnitCell {
			t.Fatalf("%s: sheets %q (%s), want %q", c.file, text.Sheets, text.Unit, c.sheets)
		}
		for id, want := range c.cells {
			s, ok := text.segment(id)
			if !ok || s.Text != want {
				t.Fatalf("%s: cell %s = %q (found %t), want %q", c.file, id, s.Text, ok, want)
			}
			if s.Sheet != c.sheets[s.SheetIndex-1] || !strings.HasSuffix(id, "!"+s.Cell) {
				t.Fatalf("%s: cell identity %+v", c.file, s)
			}
			loc := pieceLocator(text, s, 0, s.Runes, 0)
			if loc.Kind != types.LocatorXLSX || loc.Sheet != s.Sheet || loc.Cell != s.Cell || loc.Page != 0 || loc.Paragraph != 0 {
				t.Fatalf("%s: xlsx locator %+v", c.file, loc)
			}
		}
		units := map[int][]rune{}
		for _, pg := range ex.Pages {
			units[pg.Number] = []rune(pg.Text)
		}
		for _, s := range text.Segments {
			if string(units[s.unit][s.base:s.base+s.Runes]) != s.Text {
				t.Fatalf("%s: %s is not at its recorded offsets", c.file, s.ID)
			}
		}
	}
	// A € amount read from a cell is an amount the verifier recognises.
	if !AmountStatedIn(types.Money{MinorUnits: 3882287, Currency: "EUR"}, "38822.87 EUR") ||
		!AmountStatedIn(types.Money{MinorUnits: 15840, Currency: "EUR"}, "158.4 EUR") {
		t.Fatal("a currency-formatted cell must state its amount")
	}
}

func TestXLSXCanonicalCellText(t *testing.T) {
	workbook := `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<workbookPr/><sheets>` +
		`<sheet name="Synthèse" sheetId="3" r:id="rId3"/><sheet name="Détail" sheetId="1" r:id="rId1"/>` +
		`<sheet name="Masquée" sheetId="2" state="hidden" r:id="rId2"/><sheet name="Graphique" sheetId="4" r:id="rId4"/>` +
		`</sheets></workbook>`
	rels := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="/xl/worksheets/sheet2.xml"/>` +
		`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet10.xml"/>` +
		`<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chartsheet" Target="chartsheets/sheet1.xml"/>` +
		`</Relationships>`
	styles := `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<numFmts count="2"><numFmt numFmtId="164" formatCode="#,##0.00\ &quot;€&quot;"/><numFmt numFmtId="165" formatCode="dd/mm/yyyy\ hh:mm"/></numFmts>` +
		`<cellStyleXfs count="1"><xf numFmtId="164"/></cellStyleXfs>` +
		`<cellXfs count="4"><xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="14"/><xf numFmtId="165"/></cellXfs></styleSheet>`
	shared := `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<si><t>Salaire</t></si>` +
		`<si><r><t>Ligne</t></r><r><t xml:space="preserve"> un_x000D_deux</t></r><rPh sb="0" eb="1"><t>ふりがな</t></rPh></si></sst>`
	synthese := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" s="1"><v>2940</v></c><c r="C1" s="2"><v>45733</v></c><c r="D1" s="3"><v>45733.2583333333</v></c></row>` +
		`<row r="3"><c r="A3" t="s"><v>1</v></c><c t="b"><v>1</v></c><c t="e"><v>#DIV/0!</v></c><c r="E3"><f>SUM(A1:A2)</f></c><c r="F3" t="inlineStr"><is><t>Texte</t></is></c></row>` +
		`</sheetData></worksheet>`
	detail := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="2"><c r="B2"><v>17.4062</v></c></row></sheetData></worksheet>`
	hidden := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>caché</t></is></c></row></sheetData></worksheet>`
	raw := testZip(map[string][]byte{
		"xl/workbook.xml": []byte(workbook), "xl/_rels/workbook.xml.rels": []byte(rels), "xl/styles.xml": []byte(styles),
		"xl/sharedStrings.xml": []byte(shared), "xl/worksheets/sheet10.xml": []byte(synthese),
		"xl/worksheets/sheet1.xml": []byte(detail), "xl/worksheets/sheet2.xml": []byte(hidden),
	})
	ex, err := xlsxExtract(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Workbook (tab) order, never the parts' alphabetical order.
	if strings.Join(ex.Record.Sheets, "|") != "Synthèse|Détail|Masquée" {
		t.Fatalf("sheets in workbook order expected: %q", ex.Record.Sheets)
	}
	text := officeText(t, pieceFormatXLSX, ex)
	want := map[string]string{
		"s1!A1": "Salaire", "s1!B1": "2940 EUR", "s1!C1": "2025-03-17", "s1!D1": "2025-03-17T06:12",
		"s1!A3": "Ligne un\rdeux", "s1!B3": "TRUE", "s1!C3": "#DIV/0!", "s1!F3": "Texte",
		"s2!B2": "17.4062", "s3!A1": "caché",
	}
	if len(text.Segments) != len(want) {
		t.Fatalf("%d cells, want %d: %+v", len(text.Segments), len(want), text.Segments)
	}
	for id, w := range want {
		if s, ok := text.segment(id); !ok || s.Text != w {
			t.Fatalf("cell %s = %q (found %t), want %q", id, s.Text, ok, w)
		}
	}
	if ex.Pages[0].Text != "Salaire\t2940 EUR\t2025-03-17\t2025-03-17T06:12\nLigne un\rdeux\tTRUE\t#DIV/0!\tTexte" {
		t.Fatalf("sheet text: %q", ex.Pages[0].Text)
	}
	codes := map[string]ExtractionLimitation{}
	for _, l := range ex.Record.Limitations {
		codes[l.Code] = l
	}
	if codes[LimitXLSXHiddenSheet].Detail != "Masquée" || codes[LimitXLSXFormulaNoValue].Count != 1 || codes[LimitXLSXOtherSheets].Count != 1 {
		t.Fatalf("limitations: %+v", ex.Record.Limitations)
	}
}

func TestExcelNumberFormatsAndDates(t *testing.T) {
	for _, c := range []struct {
		v    float64
		d    bool
		want string
		ok   bool
	}{
		{45733, false, "2025-03-17", true},
		{1, false, "1900-01-01", true},
		{60, false, "", false}, // Excel's 29 February 1900 never existed
		{61, false, "1900-03-01", true},
		{0.5, false, "12:00", true},
		{45733.5000115741, false, "2025-03-17T12:00:01", true},
		{0, true, "1904-01-01", true},
		{-1, false, "", false},
	} {
		got, ok := excelSerialText(c.v, c.d)
		if got != c.want || ok != c.ok {
			t.Fatalf("serial %v (1904=%t) = %q/%t, want %q/%t", c.v, c.d, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		id     int
		code   string
		custom bool
		date   bool
	}{
		{14, "", false, true}, {22, "", false, true}, {4, "", false, false},
		{164, `#,##0.00\ "€"`, true, false}, {165, "dd/mm/yyyy", true, true}, {166, "[h]:mm", true, true},
		{167, "[Red]0.00", true, false}, {168, `0.00" jours"`, true, false}, {169, "mmm-yy;@", true, true},
	} {
		if got := xlsxIsDateFormat(c.id, c.code, c.custom); got != c.date {
			t.Fatalf("format %d %q: date=%t, want %t", c.id, c.code, got, c.date)
		}
	}
	for code, want := range map[string]string{`#,##0.00\ "€"`: "EUR", `[$£-809]#,##0.00`: "GBP", "0.00": "", `0.00;[Red]-0.00 "€"`: ""} {
		if got := xlsxCurrency(code); got != want {
			t.Fatalf("currency of %q = %q, want %q", code, got, want)
		}
	}
}

func TestLocatorNotation(t *testing.T) {
	for _, c := range []struct {
		loc  types.SourceLocator
		want string
	}{
		{types.SourceLocator{Kind: types.LocatorDOCX, Paragraph: 4}, "§ 4"},
		{types.SourceLocator{Kind: types.LocatorPDF, Page: 2, Paragraph: 3}, "p. 2, § 3"},
		{types.SourceLocator{Kind: types.LocatorPDF, Page: 2}, "p. 2"},
		{types.SourceLocator{Kind: types.LocatorXLSX, Sheet: "Récap", Cell: "B4"}, "Récap!B4"},
		{types.SourceLocator{Kind: types.LocatorXLSX, Sheet: "Chiffrage des demandes", Cell: "D8"}, "'Chiffrage des demandes'!D8"},
		{types.SourceLocator{Kind: types.LocatorXLSX, Sheet: "L'onglet", Cell: "A1"}, "'L''onglet'!A1"},
		{types.SourceLocator{Kind: types.LocatorXLSX, SegmentID: "s1"}, ""}, // a Slice 1 sheet locator names no cell
	} {
		if got := pieceLocatorText(c.loc); got != c.want {
			t.Fatalf("%+v reads %q, want %q", c.loc, got, c.want)
		}
	}
}

func TestPDFParagraphsTileThePage(t *testing.T) {
	page := "  En-tête\n\nPremier paragraphe\nsuite\n\n\n   \nSecond\n"
	parts := pdfParagraphParts(page)
	r := []rune(page)
	if len(parts) != 3 || parts[0].Start != 0 || parts[2].End != len(r) {
		t.Fatalf("three paragraphs tiling the page expected: %+v", parts)
	}
	for i, p := range parts {
		if i > 0 && p.Start != parts[i-1].End {
			t.Fatalf("gap or overlap before §%d", p.Paragraph)
		}
	}
	if got := strings.TrimSpace(string(r[parts[1].Start:parts[1].End])); got != "Premier paragraphe\nsuite" {
		t.Fatalf("§2 = %q", got)
	}
	if len(pdfParagraphParts(" \n\t\n")) != 0 {
		t.Fatal("a blank page has no paragraph")
	}
}

// Every passage read_piece would deliver, on every Vasseur pièce, is the
// exact text at its locator, the passages of a range tile it, and the ranges
// cover every readable segment.
func TestTextAtLocatorIsTheQuotedText(t *testing.T) {
	entries, err := os.ReadDir(vasseurDir)
	if errors.Is(err, os.ErrNotExist) {
		testmode.Missing(t, "testdata/ is untracked and absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	_, pdftotext := exec.LookPath("pdftotext")
	checked := 0
	for _, e := range entries {
		name := e.Name()
		raw := vasseurFixture(t, name)
		var text PieceText
		switch filepath.Ext(name) {
		case ".pdf":
			if pdftotext != nil {
				continue
			}
			pages, err := pdfTextPages(context.Background(), raw)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			text = PieceText{Format: pieceFormatPDF, Unit: SegmentUnitPage, State: PieceTextReady, Version: PieceTextVersion,
				Segments: pdfSegments(pages, nil), units: pages}.finish()
		case ".docx":
			ex, err := docxExtract(raw)
			if err != nil {
				t.Fatal(err)
			}
			text = officeText(t, pieceFormatDOCX, ex)
		case ".xlsx":
			ex, err := xlsxExtract(raw)
			if err != nil {
				t.Fatal(err)
			}
			text = officeText(t, pieceFormatXLSX, ex)
		default:
			continue
		}
		covered := map[string][][2]int{}
		cursor := ""
		for windows := 0; ; windows++ {
			ranges, _, next, err := text.readWindow(cursor, readPieceMin)
			if err != nil || windows > 500 {
				t.Fatalf("%s: window %d: %v", name, windows, err)
			}
			for _, rg := range ranges {
				var joined strings.Builder
				for _, ps := range text.passagesOf(rg) {
					if string(ps.Seg.runeSlice()[ps.Start:ps.End]) != ps.Text {
						t.Fatalf("%s: passage text differs from the text at [%d,%d) of %s", name, ps.Start, ps.End, ps.Seg.ID)
					}
					loc := pieceLocator(text, ps.Seg, ps.Start, ps.End, ps.Paragraph)
					if text.Format == pieceFormatPDF && (loc.Page == 0 || loc.Paragraph == 0) ||
						text.Format != pieceFormatPDF && loc.Page != 0 {
						t.Fatalf("%s: locator %+v", name, loc)
					}
					joined.WriteString(ps.Text)
				}
				if joined.String() != rg.Text {
					t.Fatalf("%s: passages of %s do not tile the delivered range", name, rg.Seg.ID)
				}
				covered[rg.Seg.ID] = append(covered[rg.Seg.ID], [2]int{rg.Start, rg.End})
			}
			if next == "" {
				break
			}
			cursor = next
		}
		for _, s := range text.Segments {
			if !s.Unreadable && !covers(covered[s.ID], s.Runes) {
				t.Fatalf("%s: %s not delivered whole", name, s.ID)
			}
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("only %d fixtures checked", checked)
	}
}

// A retrieval chunk that opens on overlap carried from the previous chunk
// points where that text is in the canonical extraction — the same place the
// previous chunk had it — and a chunk cut from older text is found by
// content at the same offsets.
func TestChunkOverlapDoesNotShiftOffsets(t *testing.T) {
	var paras []string
	for i := 1; i <= 60; i++ {
		paras = append(paras, "Paragraphe "+strconv.Itoa(i)+" : le salarié rappelle que l’équipe de nuit ne compte plus que deux caristes depuis février, "+
			"ce qui rend le comptage contradictoire impossible à réaliser avant chaque départ de tournée.")
	}
	ex, err := docxExtract(testDOCX(paras...))
	if err != nil {
		t.Fatal(err)
	}
	text := officeText(t, pieceFormatDOCX, ex)
	chunks := chunkPages(ex.Pages, chunkTargetTokens, chunkOverlapTokens)
	if len(chunks) < 3 {
		t.Fatalf("expected overlapping chunks, got %d", len(chunks))
	}
	carried := 0
	for i, ch := range chunks {
		first := strings.SplitN(ch.Text, "\n\n", 2)[0]
		seg, off, ok := text.locateChunk(ch.Text, ch.PageFrom)
		if !ok || off != 0 || seg.Text != first {
			t.Fatalf("chunk %d mapped to %v@%d, want the paragraph it opens on", i, seg, off)
		}
		if i > 0 && strings.Contains(chunks[i-1].Text, first) {
			carried++
			// The same paragraph, found inside the previous chunk, is the same
			// canonical place.
			prev, poff, ok := text.locateChunk(first, 0)
			if !ok || prev.ID != seg.ID || poff != off {
				t.Fatalf("chunk %d: carried paragraph at %s@%d, previous chunk at %s@%d", i, seg.ID, off, prev.ID, poff)
			}
		}
		// Re-flowed text (an older chunker, other whitespace) lands on the
		// same offsets through the normalised match.
		if s2, off2, ok := text.locateChunk(strings.ReplaceAll(ch.Text, " ", "  "), ch.PageFrom); !ok || s2.ID != seg.ID || off2 != off {
			t.Fatalf("chunk %d: content match drifted to %v@%d", i, s2, off2)
		}
	}
	if carried == 0 {
		t.Fatal("no chunk opened on carried overlap — the test proves nothing")
	}

	// A PDF page cut inside a long paragraph: the part's anchor is the exact
	// offset of its first word in the page.
	words := strings.Repeat("mot ", 1500)
	pages := []ingestPage{{Number: 1, Text: "Titre\n\n" + words}}
	segs := segmentPages(pages, chunkTargetTokens)
	r := []rune(pages[0].Text)
	for _, s := range segs {
		if got := strings.Join(strings.Fields(string(r[s.start:s.end])), " "); got != s.text {
			t.Fatalf("segment anchor [%d,%d) does not hold its text", s.start, s.end)
		}
	}
}

// ---------------------------------------------------------------------------
// Database: read_piece, coverage, legacy pieces, the OCR cap
// ---------------------------------------------------------------------------

func TestReadPieceReturnsTypedLocators(t *testing.T) {
	f := newToolFixture(t)
	docx := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 6, "06-transcription-sms-2025-03-17.docx",
		vasseurFixture(t, "06-transcription-sms-2025-03-17.docx"), false)
	xlsx := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 18, "18-note-de-chiffrage-demandes.xlsx",
		vasseurFixture(t, "18-note-de-chiffrage-demandes.xlsx"), false)
	c := f.contract()

	// DOCX: paragraphs, never a page.
	x := f.exec(c)
	read := f.call(x, types.ToolReadPiece, map[string]any{"piece_id": docx.ID, "cursor": nil, "max_chars": readPieceMax})
	wantOK(t, read)
	d := obsData(t, read)
	paras, _ := d["paragraphs"].([]any)
	if len(paras) != 16 || d["pages"] != nil || d["unit"] != SegmentUnitParagraph || d["text_version"] != PieceTextVersion {
		t.Fatalf("docx read must deliver 16 paragraphs and no page: %s", read.Observation.Data)
	}
	if len(read.Observation.EvidenceIDs) != 16 || len(read.Observation.ReadReceiptIDs) != 16 {
		t.Fatalf("one span and one receipt per paragraph: %d / %d", len(read.Observation.EvidenceIDs), len(read.Observation.ReadReceiptIDs))
	}
	found := false
	for _, s := range spansOf(t, f.repo(), f.runID, docx.ID) {
		loc := locatorOf(t, s)
		if loc.Kind != types.LocatorDOCX || loc.Page != 0 || loc.Paragraph == 0 || loc.SegmentID != "para"+strconv.Itoa(loc.Paragraph) {
			t.Fatalf("docx span locator %+v", loc)
		}
		if strings.Contains(s.Quote, "Fais partir le 4512") {
			found = loc.Paragraph == 5 && loc.Start == 0 && loc.End == len([]rune(s.Quote))
		}
	}
	if !found {
		t.Fatal("the 05 h 48 message must be cited at § 5, whole")
	}
	if read.Activity.Detail != "§ 1 à 16" {
		t.Fatalf("activity detail %q", read.Activity.Detail)
	}

	// XLSX: cells under their sheet's real name, read in three windows, one
	// of them overlapping another.
	x1 := f.exec(c)
	w1 := f.call(x1, types.ToolReadPiece, map[string]any{"piece_id": xlsx.ID, "cursor": nil, "max_chars": readPieceMin})
	wantOK(t, w1)
	next := w1.Observation.NextCursor
	if !strings.HasPrefix(next, "s1!") {
		t.Fatalf("a 1000-character window must stop inside the first sheet: %q", next)
	}
	sheets := obsData(t, w1)["sheets"].([]any)
	first := sheets[0].(map[string]any)
	if first["sheet"] != "Chiffrage des demandes" || first["cells"].([]any)[0].(map[string]any)["cell"] != "A1" {
		t.Fatalf("cells grouped under the sheet name: %v", first)
	}
	if !strings.HasPrefix(w1.Activity.Detail, "'Chiffrage des demandes'!A1 à ") {
		t.Fatalf("activity detail %q", w1.Activity.Detail)
	}
	x2 := f.exec(c)
	wantOK(t, f.call(x2, types.ToolReadPiece, map[string]any{"piece_id": xlsx.ID, "cursor": nil, "max_chars": readPieceMin}))
	x3 := f.exec(c)
	w3 := f.call(x3, types.ToolReadPiece, map[string]any{"piece_id": xlsx.ID, "cursor": next, "max_chars": readPieceMax})
	wantOK(t, w3)
	if obsData(t, w3)["end_of_piece"] != true {
		t.Fatal("the third window must finish the workbook")
	}
	var d8 types.AgentEvidenceSpan
	for _, s := range spansOf(t, f.repo(), f.runID, xlsx.ID) {
		if loc := locatorOf(t, s); loc.Cell == "D8" {
			d8 = s
			if loc.Kind != types.LocatorXLSX || loc.Sheet != "Chiffrage des demandes" || loc.Page != 0 || loc.Start != 0 || loc.End != 8 {
				t.Fatalf("D8 locator %+v", loc)
			}
		}
	}
	if d8.Quote != "5880 EUR" {
		t.Fatalf("D8 quote %q", d8.Quote)
	}

	consume(t, f, x1.CallID, x2.CallID)
	_, by := coverageOf(t, f, c)
	partial := by[xlsx.ID]
	if partial.State != types.CoveragePartial || partial.Unit != SegmentUnitCell || partial.Segments.Consumed == 0 {
		t.Fatalf("two overlapping first windows: partial coverage in cells: %+v", partial)
	}
	consume(t, f, x3.CallID)
	_, by = coverageOf(t, f, c)
	whole := by[xlsx.ID]
	if whole.State != types.CoverageComplete || whole.Segments.Consumed != whole.Segments.Expected {
		t.Fatalf("workbook complete, every cell counted once: %+v", whole)
	}
	var receipts int64
	f.repo().AgentReadReceipts().Where("run_id = ? AND piece_id = ?", f.runID, xlsx.ID).Count(&receipts)
	if int(receipts) <= whole.Segments.Expected {
		t.Fatalf("the overlap must have produced duplicate receipts (%d for %d cells) that coverage counts once", receipts, whole.Segments.Expected)
	}
}

func TestLegacyOfficePiecesAreReReadFromTheirOriginal(t *testing.T) {
	f := newToolFixture(t)
	for _, name := range []string{"06-transcription-sms-2025-03-17.docx", "17-recapitulatif-bulletins-de-paie.xlsx"} {
		raw := vasseurFixture(t, name)
		legacy := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 1, name, raw, true)
		fresh := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 2, name+"-v2", append([]byte(nil), raw...), false)
		lt, err := LoadPieceText(context.Background(), f.repo(), legacy, nil)
		if err != nil {
			t.Fatal(err)
		}
		ft, err := LoadPieceText(context.Background(), f.repo(), fresh, nil)
		if err != nil {
			t.Fatal(err)
		}
		if lt.State != PieceTextReady || len(lt.Segments) != len(ft.Segments) || lt.ManifestSHA256 != ft.ManifestSHA256 {
			t.Fatalf("%s: a legacy piece must get the same canonical text as a fresh one (%d vs %d segments)",
				name, len(lt.Segments), len(ft.Segments))
		}
		for i := range lt.Segments {
			if lt.Segments[i].ID != ft.Segments[i].ID || lt.Segments[i].Text != ft.Segments[i].Text || lt.Segments[i].Sheet != ft.Segments[i].Sheet {
				t.Fatalf("%s: segment %d differs", name, i)
			}
		}
	}
	// The original gone: unreadable, said, never the flattened sidecar.
	gone := addOriginalPiece(t, f.repo(), f.blob, f.dossier.ID, 3, "gone.docx", testDOCX("Paragraphe unique."), true)
	var asset types.Asset
	f.repo().Assets().Where("id = ?", gone.SourceAssetID).Take(&asset)
	delete(f.blob.objects, asset.S3Key)
	gt, err := LoadPieceText(context.Background(), f.repo(), gone, nil)
	if err != nil || gt.State != PieceTextUnreadable || gt.Reason != "original_missing" {
		t.Fatalf("a legacy piece without its original is unreadable: state=%s reason=%s err=%v", gt.State, gt.Reason, err)
	}
}

func TestScanKeepsItsOCRCapInTheManifest(t *testing.T) {
	f := newToolFixture(t)
	finished := func(p types.Piece) {
		step := types.RunStep{ID: uuid.NewString(), RunID: "ingest_" + uuid.NewString(), StepKey: StepKindChunkAndEmbed + ":" + p.ID,
			Kind: types.StepKindLLM, State: types.StepStateSucceeded, AvailableAt: time.Now(), IdempotencyKey: uuid.NewString()}
		if err := f.repo().Create(&step); err != nil {
			t.Fatal(err)
		}
		f.blob.objects[pageTextKey(f.tenant, f.dossier.ID, p.ID, 1)] = []byte("Première page lue par OCR.")
		f.blob.objects[pageTextKey(f.tenant, f.dossier.ID, p.ID, 2)] = []byte("Deuxième page lue par OCR.")
	}

	// A scan of five pages of which OCR read two; chunking rewrote
	// Piece.PageCount to 2. The record still knows there are five.
	scan := f.addPiece(pieceSpec{ordinal: 1, name: "scan.pdf", mime: mimePDF, pageCnt: 2})
	rec, _ := json.Marshal(pdfExtractionRecord(make([]ingestPage, 5), true, 2))
	f.blob.objects[extractRecordKey(f.tenant, f.dossier.ID, scan.ID)] = rec
	finished(scan)

	// The same scan ingested before records: the extract_text step's output
	// is the retained source of its page counts.
	old := f.addPiece(pieceSpec{ordinal: 2, name: "ancien-scan.pdf", mime: mimePDF, pageCnt: 2})
	out, _ := json.Marshal(extractOutput{Pages: 5, Scanned: true, OCRPages: 2, Engine: "pdftotext"})
	extract := types.RunStep{ID: uuid.NewString(), RunID: "ingest_" + uuid.NewString(), StepKey: StepKindExtractText + ":" + old.ID,
		Kind: types.StepKindOCR, State: types.StepStateSucceeded, AvailableAt: time.Now(), IdempotencyKey: uuid.NewString(), Output: types.JSONB(out)}
	if err := f.repo().Create(&extract); err != nil {
		t.Fatal(err)
	}
	finished(old)

	c := f.contract()
	for _, p := range []types.Piece{scan, old} {
		text, err := LoadPieceText(context.Background(), f.repo(), p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(text.Segments) != 5 || text.Readable() != 2 || text.ExpectedPages != 5 || text.ProcessedPages != 2 || text.OCRPageCap != maxOCRPages {
			t.Fatalf("%s: five pages, two read, the cap visible: %d segments, %d readable, %d/%d cap %d",
				p.OriginalName, len(text.Segments), text.Readable(), text.ProcessedPages, text.ExpectedPages, text.OCRPageCap)
		}
		if s, _ := text.segment("p5"); !s.Unreadable || s.Reason != SegmentOCRCap {
			t.Fatalf("%s: a page past the cap is unreadable for that reason: %+v", p.OriginalName, s)
		}
		m, err := pieceManifest(context.Background(), f.repo(), p)
		if err != nil || len(m.Segments) != 5 || m.Segments[4].Readable {
			t.Fatalf("%s: the gate's manifest must hold the unread pages: %+v %v", p.OriginalName, m, err)
		}
		read := f.call(f.exec(c), types.ToolReadPiece, map[string]any{"piece_id": p.ID, "cursor": nil, "max_chars": nil})
		wantOK(t, read)
		d := obsData(t, read)
		ext, _ := d["extraction"].(map[string]any)
		if ext["expected_pages"] != float64(5) || ext["processed_pages"] != float64(2) || d["unreadable_count"] != float64(3) {
			t.Fatalf("%s: read_piece must say three pages were never read: %s", p.OriginalName, read.Observation.Data)
		}
	}
}

// ---------------------------------------------------------------------------
// Verification and citation display at the paragraph or the cell
// ---------------------------------------------------------------------------

func TestPieceMismatchIsReportedAtTheParagraphOrCell(t *testing.T) {
	f := newAgentFixture(t, "locators")
	blob := &toolBlob{objects: map[string][]byte{}}
	prev := u.Blob
	u.Blob = blob
	t.Cleanup(func() { u.Blob = prev })

	sms := addOriginalPiece(t, f.repo, blob, f.dossier.ID, 6, "06-transcription-sms-2025-03-17.docx",
		vasseurFixture(t, "06-transcription-sms-2025-03-17.docx"), false)
	chiffrage := addOriginalPiece(t, f.repo, blob, f.dossier.ID, 18, "18-note-de-chiffrage-demandes.xlsx",
		vasseurFixture(t, "18-note-de-chiffrage-demandes.xlsx"), false)
	impl, _ := AgentTools.Lookup(types.ToolReadPiece)
	for i, p := range []types.Piece{sms, chiffrage} {
		raw, _ := json.Marshal(map[string]any{types.ProgressNoteArg: "Je lis la pièce.", "piece_id": p.ID, "cursor": nil, "max_chars": readPieceMax})
		x := ToolExecution{TenantID: f.firm, ActorID: f.user, RunID: f.run.ID, StepID: "step_" + strconv.Itoa(i), CallID: types.AgentCallID(1, i),
			Dispatch: 1, Contract: f.contract, Permissions: ToolPermissions{DossierRead: true}}
		out, err := impl.Execute(context.Background(), x, raw)
		if err != nil || out.Observation.Status == types.ObservationError || out.Persist == nil {
			t.Fatalf("read_piece %s: %v %s", p.OriginalName, err, out.Observation.Data)
		}
		f.must(f.repo.Tx(out.Persist))
	}
	spanWhere := func(p types.Piece, match func(types.SourceLocator, string) bool) types.AgentEvidenceSpan {
		t.Helper()
		for _, s := range spansOf(t, f.repo, f.run.ID, p.ID) {
			if match(locatorOf(t, s), s.Quote) {
				return s
			}
		}
		t.Fatalf("no span of %s matches", p.OriginalName)
		return types.AgentEvidenceSpan{}
	}
	order := spanWhere(sms, func(l types.SourceLocator, q string) bool { return l.Paragraph == 5 })
	preavis := spanWhere(chiffrage, func(l types.SourceLocator, q string) bool { return l.Cell == "D8" })

	claim := func(id, text string, ev string, set func(*types.DocumentClaim)) types.DocumentBlock {
		s, e := whole(text)
		c := types.DocumentClaim{ID: "c" + id, Kind: types.ClaimFactual, Start: s, End: e, EvidenceIDs: []string{ev}}
		set(&c)
		return para("b"+id, text, c)
	}
	rev := f.addRevision(types.AgentOutputAnswer, "Réponse",
		// The 05 h 48 message is real, but § 5 does not state its date.
		claim("1", "K. Bouzid ordonne le départ immédiat de la tournée 4512.", order.ID, func(c *types.DocumentClaim) {
			c.Dates = []types.ClaimDate{{PartialDate: day(2025, 3, 17), Role: types.DateRoleEvent}}
		}),
		claim("2", "Le préavis est chiffré à 5 880,00 €.", preavis.ID, func(c *types.DocumentClaim) {
			c.Amounts = []types.Money{{MinorUnits: 588000, Currency: "EUR"}}
		}),
		claim("3", "Le préavis est chiffré à 5 980,00 €.", preavis.ID, func(c *types.DocumentClaim) {
			c.Amounts = []types.Money{{MinorUnits: 598000, Currency: "EUR"}}
		}),
	)
	report := f.verify(rev.ID, false)
	if report.Blocking != 0 || !report.DownloadAllowed || report.Result != types.VerificationWarnings {
		t.Fatalf("pièce mismatches warn and keep download: result=%s download=%t blocking=%d issues=%+v",
			report.Result, report.DownloadAllowed, report.Blocking, report.Issues)
	}
	byBlock := map[string][]string{}
	for _, iss := range report.Issues {
		byBlock[iss.BlockID] = append(byBlock[iss.BlockID], iss.Correction)
		if iss.Severity != types.SeverityWarning {
			t.Fatalf("a pièce mismatch is a warning: %+v", iss)
		}
	}
	if len(byBlock["b2"]) != 0 {
		t.Fatalf("the amount the cell states must pass: %v", byBlock["b2"])
	}
	wantSentence := func(block, sentence string) {
		t.Helper()
		if !containsString(byBlock[block], sentence) {
			t.Fatalf("block %s: want %q in %q", block, sentence, byBlock[block])
		}
	}
	wantSentence("b1", "À vérifier — la date du 17 mars 2025 attribuée à la pièce n° 6 (§ 5) n'a pas été retrouvée dans le passage cité.")
	wantSentence("b3", "À vérifier — le montant de 5 980,00 € attribué à la pièce n° 18 ('Chiffrage des demandes'!D8) n'a pas été retrouvé dans le passage cité.")

	// The citation names the cell and the paragraph — never a page — and
	// shows the passage in place with the cited range highlighted exactly.
	for _, c := range []struct {
		span  types.AgentEvidenceSpan
		piece types.Piece
		label string
		near  string
	}{
		{preavis, chiffrage, ", 'Chiffrage des demandes'!D8", "Indemnité compensatrice de préavis"},
		{order, sms, ", § 5", "05 h 47 — K. BOUZID"},
	} {
		src := &spanSource{Span: c.span, Piece: &c.piece}
		if label := src.label(); !strings.HasSuffix(label, c.label) || strings.Contains(label, "p. ") {
			t.Fatalf("label %q, want it to end with %q", label, c.label)
		}
		row := types.Citation{SourceType: types.CitationSourcePiece, PieceID: &c.piece.ID, Quote: c.span.Quote, SourceSHA256: c.span.SourceSHA256}
		passage, hs, he, ok := piecePassageAround(context.Background(), f.repo, row, locatorOf(t, c.span))
		if !ok || string([]rune(passage)[hs:he]) != c.span.Quote || !strings.Contains(passage, c.near) {
			t.Fatalf("passage in place for %s: ok=%t highlight=%q", c.label, ok, string([]rune(passage)[hs:he]))
		}
		row.Quote = "texte qui n'est pas à cet endroit"
		if _, _, _, ok := piecePassageAround(context.Background(), f.repo, row, locatorOf(t, c.span)); ok {
			t.Fatal("a quote that is not at the locator must not be shown in place")
		}
	}
}
