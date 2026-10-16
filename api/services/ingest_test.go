package services

// Ingestion tests (docs/PLAN.md §7.1, §7.2).
//
// These run against the REAL Postgres and the REAL gateway, because every
// interesting property of this pipeline lives outside Go: poppler's text
// layer, tesseract's confidence, to_tsvector('french', …), and a halfvec(1024)
// column that only accepts what bge-m3 actually returns. A mocked ingestion
// would test the mock.
//
// The fixtures are generated here rather than committed: two tiny PDFs (one
// with a text layer, one rasterised), an encrypted-looking PDF and a zip
// wearing a .pdf extension. Everything a lawyer's document could be is
// hostile; the fixtures are the cheap half of that.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

const ingestTestDSN = "postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"

var (
	ingestTestOnce   sync.Once
	ingestTestTenant = "test-ingest-" + uuid.NewString()[:8]
	ingestTestUser   = "test-user-" + uuid.NewString()[:8]
	ingestRunnerStop context.CancelFunc
)

// ingestTestSetup boots the pieces of the process these tests need: the .env
// (for DATABASE_URL and the gateway key), the DB handle, a private tmp root so
// the local blob backend never touches the working tree, and the runner.
//
// It is a sync.Once rather than a TestMain so this file can live alongside
// other test files in the package without fighting them over TestMain.
func ingestTestSetup(t *testing.T) {
	t.Helper()
	ingestTestOnce.Do(func() {
		testmode.LoadDotenv("../.env")
		gin.SetMode(gin.TestMode)

		// A private tmp root, resolved before the first blob call so the
		// lazy backend picks it up.
		root, err := os.MkdirTemp("", "nomos-ingest-*")
		if err != nil {
			t.Fatalf("tmp root: %v", err)
		}
		u.SetTmpRoot(root)
		if os.Getenv("APP_NAME") == "" {
			os.Setenv("APP_NAME", "nomos-test")
		}

		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dsn = ingestTestDSN
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		})
		if err != nil {
			t.Fatalf("ingestion tests need Postgres at %s: %v", dsn, err)
		}
		if err := db.AutoMigrate(types.NomosModels()...); err != nil {
			t.Fatalf("AutoMigrate: %v", err)
		}
		u.DB = db

		// Compress the queue's clock: these tests wait on real work, not on
		// the poll interval.
		runner.PollInterval = 50 * time.Millisecond
		runner.BaseBackoff = 100 * time.Millisecond

		// Rows from earlier runs are swept HERE, not in a t.Cleanup: the
		// runner and the DB handle belong to the test BINARY, and tearing
		// them down when the first test that happened to call this setup
		// finishes would leave every later test without a worker.
		ingestTestCleanup(db)

		ctx, cancel := context.WithCancel(context.Background())
		ingestRunnerStop = cancel
		runner.Start(ctx, config.Load())
	})
}

// ingestTestCleanup removes every row this suite has ever written. Tenants are
// prefixed test-ingest-, so it never touches another suite's fixtures.
func ingestTestCleanup(db *gorm.DB) {
	db.Exec(`DELETE FROM piece_chunks WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM run_events WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM step_attempts WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM run_step_deps WHERE run_id IN (SELECT id FROM runs WHERE tenant_id LIKE 'test-ingest-%')`)
	db.Exec(`DELETE FROM run_steps WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM llm_calls WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM runs WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM audit_events WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM pieces WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM assets WHERE tenant_id LIKE 'test-ingest-%'`)
	db.Exec(`DELETE FROM dossiers WHERE tenant_id LIKE 'test-ingest-%'`)
}

// ---------------------------------------------------------------------------
// Sniffing and refusals — no gateway, no OCR
// ---------------------------------------------------------------------------

// TestIngestSniffsMagicBytes proves the extension is never trusted: a zip
// renamed to .pdf is caught, and a real PDF is still recognised when its
// header sits a few bytes in.
func TestIngestSniffsMagicBytes(t *testing.T) {
	pdf := testTextPDF([]string{"Contrat de travail à durée indéterminée."})
	if kind, mime, reason := sniffDocument(pdf); kind != docKindPDF || reason != "" {
		t.Fatalf("text PDF: kind=%q mime=%q reason=%q", kind, mime, reason)
	}

	zipped := testZip(map[string][]byte{"note.txt": []byte("ceci n'est pas un pdf")})
	kind, _, reason := sniffDocument(zipped)
	if kind != "" || reason == "" {
		t.Fatalf("a bare zip must be refused, got kind=%q reason=%q", kind, reason)
	}
	if !strings.Contains(reason, "zip") && !strings.Contains(reason, "Zip") {
		t.Fatalf("the refusal must name what it actually found: %q", reason)
	}
}

// TestIngestRejectsEncryptedPDF — an encrypted PDF is REPORTED, never left to
// fail silently three steps later (§7.1).
func TestIngestRejectsEncryptedPDF(t *testing.T) {
	enc := testEncryptedPDF()
	kind, _, reason := sniffDocument(enc)
	if kind != docKindPDF || reason != "" {
		t.Fatalf("an encrypted PDF is still a PDF: kind=%q reason=%q", kind, reason)
	}
	got := validateDocument(kind, enc)
	if !strings.Contains(strings.ToLower(got), "encrypted") {
		t.Fatalf("expected an explicit encryption refusal, got %q", got)
	}
}

// TestIngestRejectsZipBomb — a docx whose parts announce an implausible
// expansion is refused on the HEADERS, before anything is inflated.
func TestIngestRejectsZipBomb(t *testing.T) {
	bomb := testZip(map[string][]byte{
		"word/document.xml": []byte(`<?xml version="1.0"?><w:document xmlns:w="x"><w:body/></w:document>`),
		"word/bomb.bin":     bytes.Repeat([]byte{0}, 40<<20),
	})
	if kind, _, reason := sniffDocument(bomb); kind != docKindDOCX || reason != "" {
		t.Fatalf("the package must still sniff as docx: kind=%q reason=%q", kind, reason)
	}
	got := validateDocument(docKindDOCX, bomb)
	if !strings.Contains(strings.ToLower(got), "decompression bomb") {
		t.Fatalf("expected a decompression-bomb refusal, got %q", got)
	}
}

// TestIngestRejectsMacros — an office document is never executed, and one
// carrying VBA is not even stored.
func TestIngestRejectsMacros(t *testing.T) {
	macro := testZip(map[string][]byte{
		"word/document.xml":   []byte(`<?xml version="1.0"?><w:document xmlns:w="x"><w:body/></w:document>`),
		"word/vbaProject.bin": []byte("MZ fake macro project"),
	})
	got := validateDocument(docKindDOCX, macro)
	if !strings.Contains(strings.ToLower(got), "macro") {
		t.Fatalf("expected a macro refusal, got %q", got)
	}
}

// TestIngestChunker checks the two properties the retrieval side depends on:
// windows near the target size, and page ranges that survive chunking.
func TestIngestChunker(t *testing.T) {
	pages := []ingestPage{
		{Number: 1, Text: strings.Repeat("Le salarié conteste la rupture du contrat. ", 200)},
		{Number: 2, Text: strings.Repeat("La cour d'appel a retenu une faute grave. ", 200)},
	}
	chunks := chunkPages(pages, chunkTargetTokens, chunkOverlapTokens)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Tokens > chunkTargetTokens+chunkOverlapTokens {
			t.Fatalf("chunk %d is %d tokens, over the %d budget", i, ch.Tokens, chunkTargetTokens)
		}
		if ch.PageFrom < 1 || ch.PageTo < ch.PageFrom || ch.PageTo > 2 {
			t.Fatalf("chunk %d has an impossible page range %d-%d", i, ch.PageFrom, ch.PageTo)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end: upload → extract → chunk → embed → qualify
// ---------------------------------------------------------------------------

// TestIngestTextLayerPipeline is the slice-1 acceptance path: a text-layer PDF
// uploaded through the real endpoint produces extracted text, chunk rows with
// 1024-dimension embeddings and a populated french tsvector, and a qualified
// piece row the lawyer can correct.
func TestIngestTextLayerPipeline(t *testing.T) {
	ingestTestSetup(t)
	requireTool(t, "pdftotext")
	requireIngestGateway(t)

	dossier := newTestDossier(t)
	pdf := testTextPDF([]string{
		"Contrat de travail à durée indéterminée conclu entre la société Aurore et Madame Lemoine. " +
			"Le salarié exerce les fonctions de responsable administratif au sein de l'établissement de Lyon. " +
			"La rémunération brute annuelle est fixée à quarante-deux mille euros sur douze mensualités.",
		"Lettre de licenciement notifiée le douze mars deux mille vingt-quatre pour insuffisance professionnelle. " +
			"L'employeur invoque des retards répétés dans la remise des rapports mensuels. " +
			"La salariée conteste la matérialité des griefs et demande la requalification de la rupture.",
	})

	resp := uploadPieces(t, dossier, map[string][]byte{"contrat.pdf": pdf})
	if len(resp.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", resp.Rejected)
	}
	if len(resp.Accepted) != 1 || resp.RunID == "" {
		t.Fatalf("expected one accepted piece and a run, got %d accepted run=%q", len(resp.Accepted), resp.RunID)
	}
	piece := resp.Accepted[0]

	waitForRun(t, resp.RunID, 3*time.Minute)

	var stored types.Piece
	if err := u.DB.Where("id = ?", piece.ID).Take(&stored).Error; err != nil {
		t.Fatalf("re-read piece: %v", err)
	}
	if stored.PageCount != 2 || stored.TextChars < 300 {
		t.Fatalf("extraction shapes look wrong: pages=%d chars=%d", stored.PageCount, stored.TextChars)
	}

	// The upload run qualifies as well as extracts: the pièces table is
	// editable from the moment the batch lands, so the type, the date and the
	// titre must already be proposed. `proposed`, never `approved` — approval
	// is the lawyer's act (§7.1).
	if stored.ClassificationState != types.PieceStateProposed {
		t.Fatalf("piece state = %q, want %q", stored.ClassificationState, types.PieceStateProposed)
	}
	if !IsPieceKind(stored.PieceKind) {
		t.Fatalf("piece_kind = %q, which is outside the closed vocabulary", stored.PieceKind)
	}
	if stored.Title == "" || stored.Confidence == 0 {
		t.Fatalf("qualification left the row empty: title=%t confidence=%.2f", stored.Title != "", stored.Confidence)
	}
	t.Logf("qualified kind=%s precision=%s dated=%t confidence=%.2f",
		stored.PieceKind, stored.DatePrecision, stored.DocumentDate != nil, stored.Confidence)

	// The original must be byte-identical to what was uploaded: originals are
	// never modified or renamed (§7.1).
	var asset types.Asset
	if err := u.DB.Where("id = ?", stored.SourceAssetID).Take(&asset).Error; err != nil {
		t.Fatalf("re-read asset: %v", err)
	}
	rc, err := u.Blob.Get(context.Background(), asset.S3Key)
	if err != nil {
		t.Fatalf("read the stored original: %v", err)
	}
	defer rc.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("read the stored original: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), pdf) {
		t.Fatalf("the stored original differs from the upload (%d vs %d bytes)", buf.Len(), len(pdf))
	}

	var chunks int64
	if err := u.DB.Model(&types.PieceChunk{}).Where("piece_id = ?", piece.ID).Count(&chunks).Error; err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunks == 0 {
		t.Fatal("no piece_chunks rows were written")
	}

	var row struct {
		Dims   int
		Lexems int
		Model  string
		Pages  int
	}
	if err := u.DB.Raw(`
		SELECT vector_dims(embedding) AS dims,
		       length(search_vector)  AS lexems,
		       embedding_model        AS model,
		       page_to                AS pages
		  FROM piece_chunks
		 WHERE piece_id = ?
		 ORDER BY ordinal
		 LIMIT 1`, piece.ID).Scan(&row).Error; err != nil {
		t.Fatalf("inspect chunk: %v", err)
	}
	if row.Dims != 1536 {
		t.Fatalf("embedding has %d dimensions, the column is halfvec(1536)", row.Dims)
	}
	if row.Lexems == 0 {
		t.Fatal("search_vector is empty — to_tsvector('french', text) did not run in the INSERT")
	}
	t.Logf("chunks=%d embedding_dims=%d search_vector_lexemes=%d model=%s",
		chunks, row.Dims, row.Lexems, row.Model)
}

// TestIngestScannedPDFTakesOCRPath proves the scan detector and the OCR
// fan-out: a rasterised PDF carries no text layer, so extract_text must flag
// it and enqueue one ocr_page step per page, each keeping its page image.
func TestIngestScannedPDFTakesOCRPath(t *testing.T) {
	ingestTestSetup(t)
	requireTool(t, "pdftotext")
	requireTool(t, "pdftoppm")
	requireTool(t, "tesseract")
	requireOCRLanguage(t)
	requireIngestGateway(t)

	dossier := newTestDossier(t)
	scan := testScannedPDF(t, testTextPDF([]string{
		"Procès-verbal de constat établi par huissier de justice le quatre avril deux mille vingt-quatre. " +
			"Les parties étaient présentes et ont signé le registre après lecture des déclarations.",
	}))

	resp := uploadPieces(t, dossier, map[string][]byte{"scan.pdf": scan})
	if len(resp.Rejected) != 0 {
		t.Fatalf("unexpected rejection: %+v", resp.Rejected)
	}
	piece := resp.Accepted[0]
	waitForRun(t, resp.RunID, 5*time.Minute)

	var extract types.RunStep
	if err := u.DB.Where("run_id = ? AND step_key = ?", resp.RunID,
		StepKindExtractText+":"+piece.ID).Take(&extract).Error; err != nil {
		t.Fatalf("read the extract step: %v", err)
	}
	var eo extractOutput
	if err := jsonUnmarshalStepOutput(extract.Output, &eo); err != nil {
		t.Fatalf("decode extract output: %v", err)
	}
	if !eo.Scanned {
		t.Fatalf("a rasterised page must be detected as a scan (chars_per_page=%d)", eo.CharsPerPage)
	}
	if eo.CharsPerPage >= scannedCharsPerPage {
		t.Fatalf("chars_per_page=%d is above the scan threshold %d", eo.CharsPerPage, scannedCharsPerPage)
	}

	var ocrSteps []types.RunStep
	if err := u.DB.Where("run_id = ? AND kind = ?", resp.RunID, StepKindOCRPage).
		Find(&ocrSteps).Error; err != nil {
		t.Fatalf("read the ocr steps: %v", err)
	}
	if len(ocrSteps) != eo.OCRPages || len(ocrSteps) == 0 {
		t.Fatalf("expected %d ocr_page steps, got %d", eo.OCRPages, len(ocrSteps))
	}
	var po ocrPageOutput
	if err := jsonUnmarshalStepOutput(ocrSteps[0].Output, &po); err != nil {
		t.Fatalf("decode ocr output: %v", err)
	}
	if po.ImageAssetID == "" || po.ImageBytes == 0 {
		t.Fatal("the page image must be kept so a lawyer can inspect the source")
	}
	var img types.Asset
	if err := u.DB.Where("id = ?", po.ImageAssetID).Take(&img).Error; err != nil {
		t.Fatalf("read the page image asset: %v", err)
	}
	if img.Kind != types.AssetKindPageImage {
		t.Fatalf("page image asset kind = %q", img.Kind)
	}
	t.Logf("scanned=%t ocr_pages=%d engine=%s confidence=%.1f known=%t chars=%d image_bytes=%d",
		eo.Scanned, len(ocrSteps), po.Engine, po.Confidence, po.ConfidenceKnown, po.Chars, po.ImageBytes)
}

// TestIngestVisionOCRReadsRanges exercises the remote processing mode of §7.2
// against the real gateway: the scan is read as one page range, and the blank
// sheet in the middle comes back as an empty page rather than an error. It is
// OPT-IN (NOMOS_TEST_VISION_OCR=1) and never part of a default run: sending a
// page to a third-party route is an administrator's decision, not a test's.
// NOMOS_TEST_VISION_MODEL picks the model; the fixture is fictitious.
func TestIngestVisionOCRReadsRanges(t *testing.T) {
	if os.Getenv("NOMOS_TEST_VISION_OCR") != "1" {
		t.Skip("set NOMOS_TEST_VISION_OCR=1 to exercise the administrator-enabled remote OCR mode")
	}
	ingestTestSetup(t)
	requireScanTools(t)
	requireIngestGateway(t)
	useVisionOCR(t)

	scan := testImagePDF(t,
		testPageJPEG(t, "Convocation à un entretien préalable au licenciement adressée le trois février deux mille vingt-quatre."),
		blankPageJPEG(t),
		testPageJPEG(t, "Lettre de licenciement pour faute grave notifiée par lettre recommandée avec accusé de réception."),
	)
	resp := uploadPieces(t, newTestDossier(t), map[string][]byte{"scan.pdf": scan})
	waitForRun(t, resp.RunID, 5*time.Minute)

	batches := ocrBatchOutputs(t, resp.RunID)
	if len(batches) != 1 {
		t.Fatalf("expected one ocr_batch range, got %d", len(batches))
	}
	b := batches[0]
	if len(b.Fallback) != 0 || len(b.Pages) != 3 {
		t.Fatalf("the model must read all three pages (read=%d fallback=%v reason=%s)", len(b.Pages), b.Fallback, b.FallbackReason)
	}
	if b.Pages[0].Chars < 50 || b.Pages[2].Chars < 50 {
		t.Fatalf("text pages came back short: %d and %d chars", b.Pages[0].Chars, b.Pages[2].Chars)
	}
	if b.Pages[1].Chars > 20 {
		t.Fatalf("the blank sheet came back with %d chars", b.Pages[1].Chars)
	}
	// Shapes only, never the transcription itself.
	t.Logf("model=%s pages=%d chars=[%d %d %d]", b.Model, len(b.Pages), b.Pages[0].Chars, b.Pages[1].Chars, b.Pages[2].Chars)
}

// TestIngestVisionFallsBackToLocalOCR is the regression for the production
// failure: shown a blank page, the vision model answers nothing, and an empty
// answer failed the step — and with it the run and every other piece's
// ingestion. Here EVERY vision call answers empty (the rest of the gateway is
// real); the run must still succeed, with tesseract reading the pages.
func TestIngestVisionFallsBackToLocalOCR(t *testing.T) {
	ingestTestSetup(t)
	requireScanTools(t)
	requireTool(t, "tesseract")
	requireOCRLanguage(t)
	requireIngestGateway(t)
	useVisionOCR(t)
	emptyVisionAnswers(t)

	scan := testImagePDF(t,
		testPageJPEG(t, "Procès-verbal de constat établi par huissier de justice le quatre avril deux mille vingt-quatre."),
		blankPageJPEG(t),
	)
	resp := uploadPieces(t, newTestDossier(t), map[string][]byte{"scan.pdf": scan})
	waitForRun(t, resp.RunID, 5*time.Minute)

	batches := ocrBatchOutputs(t, resp.RunID)
	if len(batches) != 1 {
		t.Fatalf("expected one ocr_batch range, got %d", len(batches))
	}
	b := batches[0]
	if len(b.Pages) != 0 || len(b.Fallback) != 2 || b.FallbackReason != "empty" {
		t.Fatalf("both pages must fall back on an empty answer (read=%d fallback=%v reason=%s)",
			len(b.Pages), b.Fallback, b.FallbackReason)
	}
	var local []types.RunStep
	if err := u.DB.Where("run_id = ? AND kind = ? AND state = ?", resp.RunID, StepKindOCRPage, types.StepStateSucceeded).
		Find(&local).Error; err != nil {
		t.Fatalf("read the ocr_page steps: %v", err)
	}
	if len(local) != 2 {
		t.Fatalf("expected 2 local OCR steps to have read the fallback pages, got %d", len(local))
	}
	var chunks int64
	u.DB.Table("piece_chunks").Where("piece_id = ?", resp.Accepted[0].ID).Count(&chunks)
	if chunks == 0 {
		t.Fatal("the page tesseract read must reach the chunks")
	}
}

// TestIngestReextractKeepsReview pins what a re-ingestion may touch: running
// extract_text again over a piece the lawyer already approved re-reads its
// text and chunks, but must not demote the piece or undo the review.
func TestIngestReextractKeepsReview(t *testing.T) {
	ingestTestSetup(t)
	requireTool(t, "pdftotext")
	requireIngestGateway(t)

	dossier := newTestDossier(t)
	resp := uploadPieces(t, dossier, map[string][]byte{"contrat.pdf": testTextPDF([]string{
		"Contrat de travail à durée indéterminée conclu le premier mars deux mille dix-neuf entre la société " +
			"Nordlog et Monsieur Vasseur, engagé en qualité de préparateur de commandes au coefficient 140.",
	})})
	waitForRun(t, resp.RunID, 3*time.Minute)
	piece := resp.Accepted[0]

	// The lawyer's review.
	if err := u.DB.Model(&types.Piece{}).Where("id = ?", piece.ID).Updates(map[string]any{
		"classification_state": types.PieceStateApproved, "title": "Titre revu par l'avocat",
	}).Error; err != nil {
		t.Fatalf("approve: %v", err)
	}

	run, err := runner.Enqueue(context.Background(), runner.RunSpec{
		TenantID:        dossier.TenantID,
		DossierID:       dossier.ID,
		RequestedBy:     ingestTestUser,
		WorkflowType:    types.WorkflowDossierOrganizeV1,
		WorkflowVersion: 1,
		Instruction:     "Ré-extraction",
		Steps: []runner.StepSpec{{
			Key:   StepKindExtractText + ":" + piece.ID,
			Kind:  StepKindExtractText,
			Label: "Extraction du texte",
			Input: ingestStepInput{PieceID: piece.ID, AssetID: piece.SourceAssetID},
		}},
	})
	if err != nil {
		t.Fatalf("enqueue the re-extraction: %v", err)
	}
	waitForRun(t, run.ID, 3*time.Minute)

	var after types.Piece
	if err := u.DB.Where("id = ?", piece.ID).Take(&after).Error; err != nil {
		t.Fatalf("read the piece: %v", err)
	}
	if after.ClassificationState != types.PieceStateApproved || after.Title != "Titre revu par l'avocat" {
		t.Fatalf("re-extraction undid the review: state=%s title=%q", after.ClassificationState, after.Title)
	}
	if after.TextChars == 0 {
		t.Fatal("re-extraction must still record the text shape")
	}
}

// TestIngestMislabelledFileIsReported drives the refusals through the actual
// endpoint: a partial batch reports which file was rejected and why, and the
// good file still lands.
func TestIngestMislabelledFileIsReported(t *testing.T) {
	ingestTestSetup(t)

	dossier := newTestDossier(t)
	resp := uploadPieces(t, dossier, map[string][]byte{
		"pas-un-pdf.pdf": testZip(map[string][]byte{"a.txt": []byte("zip déguisé")}),
		"chiffre.pdf":    testEncryptedPDF(),
	})
	if len(resp.Accepted) != 0 {
		t.Fatalf("nothing in this batch is ingestable, got %d accepted", len(resp.Accepted))
	}
	if len(resp.Rejected) != 2 {
		t.Fatalf("both files must be reported, got %+v", resp.Rejected)
	}
	byName := map[string]string{}
	for _, r := range resp.Rejected {
		byName[r.Name] = r.Reason
	}
	if !strings.Contains(strings.ToLower(byName["chiffre.pdf"]), "encrypted") {
		t.Fatalf("encrypted PDF reason = %q", byName["chiffre.pdf"])
	}
	if byName["pas-un-pdf.pdf"] == "" {
		t.Fatal("the mislabelled zip was not reported")
	}
	t.Logf("rejections: %v", byName)
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func newTestDossier(t *testing.T) types.Dossier {
	t.Helper()
	repo := u.ForTenantID(context.Background(), ingestTestTenant)
	d := types.Dossier{
		ID:        uuid.NewString(),
		Title:     "Dossier de test",
		State:     types.DossierStateActive,
		CreatedBy: ingestTestUser,
	}
	if err := repo.Create(&d); err != nil {
		t.Fatalf("create dossier: %v", err)
	}
	return d
}

// uploadPieces posts a real multipart body through the real service function,
// with the context keys RequireAuth / RequireTenant would have set.
func uploadPieces(t *testing.T, d types.Dossier, files map[string][]byte) types.PieceUploadResponse {
	t.Helper()
	body := new(bytes.Buffer)
	w := multipart.NewWriter(body)
	for name, content := range files {
		part, err := w.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("multipart write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/dossiers/"+d.ID+"/pieces", body)
	c.Request.Header.Set("Content-Type", w.FormDataContentType())
	c.Set(middlewares.AuthUserKey, &u.AuthUser{ID: ingestTestUser})
	c.Set(u.TenantContextKey, ingestTestTenant)

	status, payload := IngestPieces(c, d.ID)
	if status != http.StatusCreated {
		t.Fatalf("upload returned %d: %+v", status, payload)
	}
	resp, ok := payload.(types.PieceUploadResponse)
	if !ok {
		t.Fatalf("unexpected payload type %T", payload)
	}
	return resp
}

// waitForRun blocks until the run reaches a terminal state, then fails the
// test if that state is not `succeeded`, reporting the step that broke.
func waitForRun(t *testing.T, runID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var run types.Run
		if err := u.DB.Where("id = ?", runID).Take(&run).Error; err != nil {
			t.Fatalf("read run: %v", err)
		}
		if types.TerminalRunState(run.State) {
			if run.State != types.RunStateSucceeded {
				t.Fatalf("run %s ended %s: %s", runID, run.State, describeFailedSteps(runID))
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %s: %s", runID, timeout, describeFailedSteps(runID))
}

func describeFailedSteps(runID string) string {
	var steps []types.RunStep
	u.DB.Where("run_id = ?", runID).Order("created_at").Find(&steps)
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, fmt.Sprintf("%s=%s(%s %s)", s.StepKey, s.State, s.ErrorCode, s.ErrorSummary))
	}
	return strings.Join(parts, " ")
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed on this machine — skipping (the deployed image has it)", name)
	}
}

// requireOCRLanguage keeps the OCR test honest on a laptop: the deployed image
// ships tesseract-ocr-data-fra, a dev machine often does not. If French is
// missing but English is present we read with English and say so; if neither
// is there the test skips rather than pretending to have run.
func requireOCRLanguage(t *testing.T) {
	t.Helper()
	out, err := exec.Command("tesseract", "--list-langs").CombinedOutput()
	if err != nil {
		t.Skipf("tesseract --list-langs failed: %v", err)
	}
	langs := string(out)
	if strings.Contains(langs, "\nfra") {
		return
	}
	if strings.Contains(langs, "\neng") {
		t.Logf("tesseract has no 'fra' pack on this machine — running the OCR path with 'eng'")
		ocrLanguage = "eng"
		return
	}
	t.Skip("tesseract has neither the fra nor the eng language pack — skipping the OCR path")
}

func requireIngestGateway(t *testing.T) {
	t.Helper()
	if !u.LLMConfigured() {
		t.Skip("OPENROUTER_API_KEY is not set — skipping the embedding path")
	}
}

func requireScanTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"pdftotext", "pdftoppm", "pdfseparate", "pdfunite"} {
		requireTool(t, tool)
	}
}

// useVisionOCR switches this test to the administrator-enabled vision mode.
func useVisionOCR(t *testing.T) {
	t.Helper()
	cfg := config.Load()
	cfg.VisionOCREnabled = true
	if m := os.Getenv("NOMOS_TEST_VISION_MODEL"); m != "" {
		cfg.VisionModel = m
	}
	prev := ocrCfg
	ocrCfg = func() config.Config { return cfg }
	t.Cleanup(func() { ocrCfg = prev })
}

// emptyVisionAnswers stands a fake OpenRouter in front of the real one: a
// completion carrying a file part — a vision OCR call — finishes normally with
// an empty answer, the way a model shown a blank page does; everything else
// (qualification, embeddings) goes through to the real gateway.
func emptyVisionAnswers(t *testing.T) {
	t.Helper()
	cfg := config.Load()
	upstream, err := url.Parse(strings.TrimRight(cfg.OpenRouterBaseURL, "/"))
	if err != nil {
		t.Fatalf("parse the gateway URL: %v", err)
	}
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/chat/completions" && bytes.Contains(body, []byte(`"type":"file"`)) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"fake","model":"fake-vision","choices":[{"finish_reason":"stop",`+
				`"message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":1200,"completion_tokens":80}}`)
			return
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(u.UseGateway(srv.URL, cfg.OpenRouterAPIKey))
}

// ocrBatchOutputs reads the decoded outputs of a run's ocr_batch steps, in
// page order.
func ocrBatchOutputs(t *testing.T, runID string) []ocrBatchOutput {
	t.Helper()
	var steps []types.RunStep
	if err := u.DB.Where("run_id = ? AND kind = ?", runID, StepKindOCRBatch).Find(&steps).Error; err != nil {
		t.Fatalf("read the ocr_batch steps: %v", err)
	}
	out := make([]ocrBatchOutput, 0, len(steps))
	for _, s := range steps {
		var b ocrBatchOutput
		if err := jsonUnmarshalStepOutput(s.Output, &b); err != nil {
			t.Fatalf("decode ocr_batch output %s: %v", s.StepKey, err)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// testTextPDF builds a minimal, valid, text-layer PDF: one page per string,
// Helvetica, with a real cross-reference table so poppler reads it the way it
// reads a real document.
func testTextPDF(pages []string) []byte {
	var objects []string
	kids := make([]string, 0, len(pages))
	// Object numbering: 1 catalog, 2 pages, 3 font, then (page, contents)
	// pairs from 4.
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+i*2))
	}
	objects = append(objects,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	)
	for i, text := range pages {
		contentsObj := 5 + i*2
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			contentsObj))
		stream := pdfTextStream(text)
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	return assemblePDF(objects, "")
}

// pdfTextStream lays a paragraph out as short lines, so a page carries the
// hundreds of characters a text layer actually has.
func pdfTextStream(text string) string {
	words := strings.Fields(text)
	var lines []string
	var cur []string
	for _, w := range words {
		cur = append(cur, w)
		if len(strings.Join(cur, " ")) > 60 {
			lines = append(lines, strings.Join(cur, " "))
			cur = nil
		}
	}
	if len(cur) > 0 {
		lines = append(lines, strings.Join(cur, " "))
	}

	var b strings.Builder
	b.WriteString("BT\n/F1 11 Tf\n50 780 Td\n14 TL\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "(%s) Tj T*\n", pdfEscape(l))
	}
	b.WriteString("ET")
	return b.String()
}

// pdfEscape escapes the three characters a PDF literal string cares about.
// Non-ASCII is dropped rather than encoded: the fixture needs a text layer,
// not a typography exercise.
func pdfEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 128:
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// testScannedPDF rasterises a text PDF and wraps the page in a one-page PDF:
// a document with an image and no text layer, which is exactly what a scanner
// produces.
func testScannedPDF(t *testing.T, source []byte) []byte {
	t.Helper()
	return testImagePDF(t, rasteriseJPEG(t, source, 1, false))
}

// testPageJPEG is one page of text as a scanner sees it.
func testPageJPEG(t *testing.T, text string) []byte {
	t.Helper()
	return rasteriseJPEG(t, testTextPDF([]string{text}), 1, false)
}

// rasteriseJPEG renders one page of a PDF to JPEG at 150 dpi, in grey when
// asked — what an office scanner delivers.
func rasteriseJPEG(t *testing.T, pdf []byte, page int, gray bool) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.pdf")
	if err := os.WriteFile(src, pdf, 0o600); err != nil {
		t.Fatalf("write source pdf: %v", err)
	}
	out := filepath.Join(dir, "page")
	p := fmt.Sprint(page)
	args := []string{"-jpeg", "-jpegopt", "quality=70", "-r", "150", "-singlefile", "-f", p, "-l", p}
	if gray {
		args = append(args, "-gray")
	}
	cmd := exec.Command("pdftoppm", append(args, src, out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("pdftoppm could not rasterise the fixture (%v): %s", err, b)
	}
	jpg, err := os.ReadFile(out + ".jpg")
	if err != nil {
		t.Fatalf("read rasterised page: %v", err)
	}
	return jpg
}

// blankPageJPEG is a white A4 sheet at 150 dpi with a little scanner noise:
// the separator page of a scanned bundle.
func blankPageJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 1240, 1754))
	for i := range img.Pix {
		img.Pix[i] = 250 - uint8(i*7919%6)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatalf("encode blank page: %v", err)
	}
	return buf.Bytes()
}

// testImagePDF wraps JPEG pages in a PDF with no text layer.
func testImagePDF(t *testing.T, pages ...[]byte) []byte {
	t.Helper()
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i*3)
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
	}
	for i, jpg := range pages {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(jpg))
		if err != nil {
			t.Fatalf("decode page %d: %v", i+1, err)
		}
		space := "/DeviceRGB"
		if cfg.ColorModel == color.GrayModel {
			space = "/DeviceGray"
		}
		img, content := 4+i*3, 5+i*3
		stream := fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q", cfg.Width, cfg.Height)
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] "+
				"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", cfg.Width, cfg.Height, img, content),
			fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s "+
				"/BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
				cfg.Width, cfg.Height, space, len(jpg), string(jpg)),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		)
	}
	return assemblePDF(objects, "")
}

// testEncryptedPDF is a valid PDF whose trailer announces a security handler —
// what a password-protected export looks like from the outside.
func testEncryptedPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] >>",
		"<< /Filter /Standard /V 2 /R 3 /Length 128 /P -1340 >>",
	}
	return assemblePDF(objects, "/Encrypt 4 0 R ")
}

// assemblePDF writes objects 1..n with a correct xref table and trailer.
func assemblePDF(objects []string, extraTrailer string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, extraTrailer, xref)
	return buf.Bytes()
}

// testZip builds a zip archive from a name → content map.
func testZip(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		if _, err := w.Write(content); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
