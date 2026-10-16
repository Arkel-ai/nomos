package services

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Rendering, export and quote-matching tests.

// init redirects the tmp root out of the working tree. TestMain belongs to
// services/conversations_test.go, so this file does its setup here — job
// directories and the local blob backend are both rooted at u.TmpRoot().
func init() {
	dir, err := os.MkdirTemp("", "nomos-render-test-")
	if err != nil {
		panic(err)
	}
	u.SetTmpRoot(dir)
}

// requireLocalBlob keeps a test that writes objects off a real bucket. The
// local backend is a development convenience (utils/blob.go) and it is the one
// these tests are allowed to touch.
func requireLocalBlob(t *testing.T) {
	t.Helper()
	if u.Blob.Backend() != "local" {
		t.Skipf("blob backend is %q — this test only writes to the local backend", u.Blob.Backend())
	}
}

func requirePandoc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc is not installed on this machine — the api image ships pandoc-cli (docs/PLAN.md §7.3); " +
			"install it locally with `brew install pandoc` / `apk add pandoc-cli` to run this test")
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func fixturePiece(ordinal int, title, original, kind string, ts *int64, precision string) types.Piece {
	return types.Piece{
		ID:            "piece_" + title,
		Ordinal:       ordinal,
		Title:         title,
		OriginalName:  original,
		PieceKind:     kind,
		DocumentDate:  ts,
		DatePrecision: precision,
	}
}

func unixOf(t *testing.T, iso string) *int64 {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", iso)
	if err != nil {
		t.Fatalf("bad fixture date %q: %v", iso, err)
	}
	ts := parsed.Unix()
	return &ts
}

// ---------------------------------------------------------------------------
// Citations — quote matching
// ---------------------------------------------------------------------------

func TestCitationQuoteMatchingToleratesPDFArtefacts(t *testing.T) {
	// A PDF text layer wraps lines, uses typographic apostrophes and
	// non-breaking spaces. None of that is a different quote.
	source := "à l'initiative\nde l'employeur doit être justifiée par une cause réelle et sérieuse"
	if !QuoteOccursIn("à l’initiative de l’employeur doit être justifiée par une cause réelle", source) {
		t.Error("quote matching must survive line wrapping, typographic apostrophes and NBSP")
	}
	if QuoteOccursIn("une cause sérieuse et réelle", source) {
		t.Error("quote matching must NOT accept reordered words — that is a paraphrase")
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestRenderBordereauProducesValidDocx(t *testing.T) {
	requirePandoc(t)

	doc := BordereauDoc{
		DossierTitle:    "Dupont c/ Société Alpha",
		ClientReference: "2024-0142",
		MatterType:      "contentieux prud'homal",
		GeneratedOn:     FrenchDate(time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)),
		DraftNotice:     DraftNotice,
		Pieces: []BordereauPiece{
			{Ordinal: 1, Title: "Contrat de travail", Date: "12 avril 2023", Kind: "contrat", FileName: "Pièce n°01 - 2023-04-12 - Contrat de travail.pdf"},
			{Ordinal: 2, Title: "Lettre de licenciement", Date: "3 septembre 2024", Kind: "courrier", FileName: "Pièce n°02 - 2024-09-03 - Lettre de licenciement.pdf"},
		},
		Footnotes: []string{"2 citation(s) vérifiées automatiquement au moment de l'export."},
	}

	res, err := RenderBordereau(context.Background(), doc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if err := ValidateOOXML(res.Bytes); err != nil {
		t.Fatalf("produced package failed validation: %v", err)
	}
	if res.TemplateVersion == "" || res.RendererVersion == "" || res.ReferenceSHA256 == "" {
		t.Errorf("provenance must travel with the render: %+v", res)
	}

	// A real file on this machine, so the docx can be opened and inspected.
	out := filepath.Join(u.TmpRoot(), "bordereau.docx")
	if err := os.WriteFile(out, res.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes, %s, reference %s)", out, len(res.Bytes), res.RendererVersion, res.ReferenceSHA256[:12])

	zr, err := zip.NewReader(bytes.NewReader(res.Bytes), int64(len(res.Bytes)))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	t.Logf("OOXML entries: %s", strings.Join(names, " "))

	document := mustEntry(t, zr, "word/document.xml")
	for _, want := range []string{"Titre1", "Corps", "Mention"} {
		if !bytes.Contains(document, []byte(`w:val="`+want+`"`)) {
			t.Errorf("expected the document to use the %q style", want)
		}
	}
	if !bytes.Contains(document, []byte("vérification de l")) {
		t.Error("an unapproved export must carry the 'projet — vérification de l'avocat requise' stamp")
	}
	styles := mustEntry(t, zr, "word/styles.xml")
	for _, want := range []string{"Titre1", "Corps", "Mention", "PiedDePage"} {
		if !bytes.Contains(styles, []byte(`w:styleId="`+want+`"`)) {
			t.Errorf("style %q must resolve in word/styles.xml", want)
		}
	}
}

func TestRenderEscapesMarkupInPieceTitles(t *testing.T) {
	requirePandoc(t)
	// A piece title is user/LLM data. It must reach pandoc inert: no image
	// reference, no raw HTML, no table-cell break.
	hostile := `![x](/etc/passwd) | <script>alert(1)</script> ::: {custom-style="Mention"}`
	res, err := RenderBordereau(context.Background(), BordereauDoc{
		DossierTitle: hostile,
		GeneratedOn:  "27 août 2026",
		Pieces:       []BordereauPiece{{Ordinal: 1, Title: hostile, Date: "sans date", Kind: "—", FileName: "a.pdf"}},
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(res.Bytes), int64(len(res.Bytes)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "word/media/") {
			t.Fatalf("pandoc embedded a file referenced from a piece title: %s", f.Name)
		}
	}
	document := mustEntry(t, zr, "word/document.xml")
	if bytes.Contains(document, []byte(`w:val="Mention"`)) {
		t.Error("a piece title must not be able to open a styled div")
	}
	if !bytes.Contains(document, []byte("script")) {
		t.Error("the hostile text should still appear, as literal text")
	}
}

func TestRenderValidateOOXMLRejectsUnresolvedStyle(t *testing.T) {
	pkg := synthesizeDocx(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types/>`,
		"_rels/.rels":         `<?xml version="1.0"?><Relationships/>`,
		"word/document.xml":   `<?xml version="1.0"?><w:document xmlns:w="w"><w:p><w:pPr><w:pStyle w:val="Titre1"/></w:pPr></w:p></w:document>`,
		"word/styles.xml":     `<?xml version="1.0"?><w:styles xmlns:w="w"><w:style w:styleId="Normal"/></w:styles>`,
	})
	err := ValidateOOXML(pkg)
	if err == nil || !strings.Contains(err.Error(), "Titre1") {
		t.Fatalf("validation must catch a style the document uses but styles.xml does not define, got %v", err)
	}
	t.Logf("validation refusal: %v", err)
}

func TestRenderValidateOOXMLRejectsMissingPart(t *testing.T) {
	pkg := synthesizeDocx(t, map[string]string{"word/document.xml": `<a/>`})
	if err := ValidateOOXML(pkg); !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("expected ErrInvalidPackage, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Export — naming and archive layout
// ---------------------------------------------------------------------------

func TestExportFileNameFrenchLabelFormat(t *testing.T) {
	cases := []struct {
		name  string
		piece types.Piece
		ord   int
		want  string
	}{
		{
			name:  "full date",
			piece: fixturePiece(1, "Contrat de travail", "scan001.PDF", "contrat", unixOf(t, "2023-04-12"), types.DatePrecisionDay),
			ord:   1,
			want:  "Pièce n°01 - 2023-04-12 - Contrat de travail.pdf",
		},
		{
			name:  "month precision never invents a day",
			piece: fixturePiece(2, "Bulletins de paie", "b.pdf", "bulletin", unixOf(t, "2024-03-01"), types.DatePrecisionMonth),
			ord:   2,
			want:  "Pièce n°02 - 2024-03 - Bulletins de paie.pdf",
		},
		{
			name:  "no legible date drops the segment",
			piece: fixturePiece(3, "Attestation", "a.pdf", "attestation", nil, types.DatePrecisionUnknown),
			ord:   3,
			want:  "Pièce n°03 - Attestation.pdf",
		},
		{
			name:  "path separators cannot escape the archive",
			piece: fixturePiece(4, "../../etc/passwd", "x.pdf", "", nil, types.DatePrecisionUnknown),
			ord:   12,
			want:  "Pièce n°12 - etc-passwd.pdf",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExportFileName(tc.piece, tc.ord); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestExportFrenchPieceDate(t *testing.T) {
	cases := []struct {
		ts        *int64
		precision string
		want      string
	}{
		{unixOf(t, "2024-09-03"), types.DatePrecisionDay, "3 septembre 2024"},
		{unixOf(t, "2024-09-01"), types.DatePrecisionDay, "1er septembre 2024"},
		{unixOf(t, "2024-09-15"), types.DatePrecisionMonth, "septembre 2024"},
		{unixOf(t, "2024-09-15"), types.DatePrecisionYear, "2024"},
		{nil, types.DatePrecisionUnknown, "sans date"},
	}
	for _, tc := range cases {
		if got := FrenchPieceDate(tc.ts, tc.precision); got != tc.want {
			t.Errorf("FrenchPieceDate(%v, %q) = %q, want %q", tc.ts, tc.precision, got, tc.want)
		}
	}
}

func TestExportZipCarriesRenamedOriginalsInApprovedOrder(t *testing.T) {
	requirePandoc(t)
	requireLocalBlob(t)
	ctx := context.Background()

	// Synthetic pieces, stored the way ingestion stores them: original bytes,
	// original key, never renamed.
	tenant, dossier := "firm_test", "dossier_test"
	originals := []struct {
		key  string
		body string
	}{
		{u.DossierKey(tenant, dossier, "uuid1-scan001.pdf"), "%PDF-1.4 contrat"},
		{u.DossierKey(tenant, dossier, "uuid2-scan002.pdf"), "%PDF-1.4 licenciement"},
	}
	for _, o := range originals {
		if err := u.Blob.Put(ctx, o.key, strings.NewReader(o.body), int64(len(o.body)), "application/pdf"); err != nil {
			t.Fatalf("staging %s: %v", o.key, err)
		}
	}

	entries := []ExportEntry{
		{PieceID: "p1", Ordinal: 1, Name: "Pièce n°01 - 2023-04-12 - Contrat de travail.pdf", Original: "scan001.pdf"},
		{PieceID: "p2", Ordinal: 2, Name: "Pièce n°02 - 2024-09-03 - Lettre de licenciement.pdf", Original: "scan002.pdf"},
	}
	keys := map[string]string{"p1": originals[0].key, "p2": originals[1].key}

	bordereau, err := RenderBordereau(ctx, BordereauDoc{
		DossierTitle: "Dupont c/ Société Alpha",
		GeneratedOn:  FrenchDate(time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)),
		DraftNotice:  DraftNotice,
		Pieces: []BordereauPiece{
			{Ordinal: 1, Title: "Contrat de travail", Date: "12 avril 2023", Kind: "contrat", FileName: entries[0].Name},
			{Ordinal: 2, Title: "Lettre de licenciement", Date: "3 septembre 2024", Kind: "courrier", FileName: entries[1].Name},
		},
	})
	if err != nil {
		t.Fatalf("bordereau render failed: %v", err)
	}

	archive, err := assembleZip(ctx, entries, keys, bordereau.Bytes, []acteFile{{Name: "Note de synthèse.docx", Bytes: bordereau.Bytes}})
	if err != nil {
		t.Fatalf("zip build failed: %v", err)
	}

	out := filepath.Join(u.TmpRoot(), "export.zip")
	if err := os.WriteFile(out, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", out, len(archive))

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("archive is not readable: %v", err)
	}
	want := []string{
		"Pièce n°01 - 2023-04-12 - Contrat de travail.pdf",
		"Pièce n°02 - 2024-09-03 - Lettre de licenciement.pdf",
		"bordereau.docx",
		"actes/Note de synthèse.docx",
	}
	if len(zr.File) != len(want) {
		t.Fatalf("expected %d entries, got %d", len(want), len(zr.File))
	}
	for i, f := range zr.File {
		t.Logf("zip entry %d: %s (%d bytes)", i, f.Name, f.UncompressedSize64)
		if f.Name != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, f.Name, want[i])
		}
	}

	// The originals travel byte-identical under their new names.
	body := mustEntry(t, zr, want[0])
	if string(body) != originals[0].body {
		t.Errorf("the original bytes must be copied verbatim, got %q", body)
	}
	if err := ValidateOOXML(mustEntry(t, zr, "bordereau.docx")); err != nil {
		t.Errorf("the bordereau inside the archive must be a valid OOXML package: %v", err)
	}

	// And the stored-object validation accepts exactly this manifest.
	key := u.DossierKey(tenant, dossier, "tmp-export-test.zip")
	if err := u.Blob.Put(ctx, key, bytes.NewReader(archive), int64(len(archive)), "application/zip"); err != nil {
		t.Fatal(err)
	}
	if err := validateStoredZip(ctx, key, want); err != nil {
		t.Fatalf("validateStoredZip rejected a good archive: %v", err)
	}
	if err := validateStoredZip(ctx, key, append(want, "surplus.txt")); err == nil {
		t.Error("validateStoredZip must reject an archive that does not match the manifest")
	}
}

func TestExportUniqueNameAvoidsCollisions(t *testing.T) {
	used := map[string]int{}
	first := uniqueName(used, "Pièce n°01 - Attestation.pdf")
	second := uniqueName(used, "Pièce n°01 - Attestation.pdf")
	if first == second {
		t.Fatalf("two identically named pieces must not collide in the archive: %q", second)
	}
	if second != "Pièce n°01 - Attestation (2).pdf" {
		t.Errorf("got %q", second)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assembleZip is buildExportZip with the storage lookup already done, so the
// archive layout is testable without a database.
func assembleZip(ctx context.Context, entries []ExportEntry, keys map[string]string, bordereau []byte, actes []acteFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		src, err := u.Blob.Get(ctx, keys[e.PieceID])
		if err != nil {
			return nil, err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate})
		if err != nil {
			src.Close()
			return nil, err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := writeZipEntry(zw, bordereauEntry, bordereau); err != nil {
		return nil, err
	}
	for _, a := range actes {
		if err := writeZipEntry(zw, acteDir+a.Name, a.Bytes); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func synthesizeDocx(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/styles.xml"} {
		body, ok := parts[name]
		if !ok {
			continue
		}
		if err := writeZipEntry(zw, name, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustEntry(t *testing.T, zr *zip.Reader, name string) []byte {
	t.Helper()
	f, err := zr.Open(name)
	if err != nil {
		t.Fatalf("entry %q missing: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
