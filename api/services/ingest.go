package services

// Document ingestion (docs/PLAN.md §7.1, §7.2, §7.4).
//
// POST /api/dossiers/:id/pieces takes n multipart files and, PER FILE, in this
// order: sniff the type from the MAGIC BYTES (never the extension), hash the
// original BEFORE parsing, refuse an encrypted PDF explicitly, bound a zip
// (docx/xlsx) against decompression bombs, refuse active content, store the
// UNMODIFIED original under <app>/<tenant>/<dossier>/, then write the asset +
// piece rows and enqueue the ingestion steps.
//
// Two rules outrank convenience here:
//
//   - ORIGINALS ARE NEVER MODIFIED OR RENAMED. Everything derived (extracted
//     text, page images) is a separate object; renaming is metadata applied at
//     export only.
//   - A REJECTED FILE IS REPORTED, NEVER SILENTLY DROPPED. A partial batch
//     comes back as {accepted, rejected[{name, reason}]} so the lawyer knows
//     exactly which document did not make it and why.
//
// The gap we state rather than hide: there is no antivirus in the allowed
// architecture. Office documents are parsed as zip+XML in pure Go and are
// never executed, and no external tool is ever handed a shell string.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var ingestLog = u.NewLogger("services.ingest")

// Step kinds this file and its neighbours register. They are exported because
// a workflow definition (§4.1) names them when it parameterises its plan.
const (
	StepKindExtractText   = "extract_text"
	StepKindOCRPage       = "ocr_page"
	StepKindOCRBatch      = "ocr_batch"
	StepKindChunkAndEmbed = "chunk_and_embed"
)

// Ingestion bounds. Every one of them exists to stop a single upload from
// consuming the pod: an oversized original, a thousand-file batch, a zip that
// expands to 40 GB, or a document that decompresses to more RAM than we have.
const (
	maxIngestFileBytes  = 100 << 20 // one original
	maxIngestBatchFiles = 50        // files per request
	maxIngestZipEntries = 4096      // entries in a docx/xlsx package
	maxIngestZipBytes   = 200 << 20 // total declared decompressed size
	maxIngestZipRatio   = 200       // per-entry compression ratio ceiling
	maxIngestXMLBytes   = 64 << 20  // one XML part inside the package

	// scannedCharsPerPage is the scan threshold (§7.2). A text-layer PDF
	// carries hundreds to thousands of characters per page; a rasterised
	// page carries a handful of stray glyphs, or none.
	scannedCharsPerPage = 150
)

// ingestStepInput is the jsonb payload every ingestion step carries. It holds
// ids and shapes only — never document text (§7.4).
type ingestStepInput struct {
	PieceID   string `json:"piece_id"`
	AssetID   string `json:"asset_id"`
	Page      int    `json:"page,omitempty"`
	PageTo    int    `json:"page_to,omitempty"` // last page of an ocr_batch range
	PageCount int    `json:"page_count,omitempty"`
	Scanned   bool   `json:"scanned,omitempty"`
	// Dependents names the steps ALREADY in the plan that read this piece's
	// extracted text. The workflow definition supplies them because it is the
	// thing that knows its own step keys; extract_text uses them only when it
	// discovers a scan, to make those steps wait for the OCR fan-out it just
	// enqueued. Without it, qualify_piece is claimable the instant extraction
	// succeeds and reads a text sidecar that OCR has not written yet.
	Dependents []string `json:"dependents,omitempty"`
}

func init() {
	// extract_text shells out to pdftotext, so it belongs to the OCR pool
	// (concurrency 1 per pod) and not the db pool: a CPU-bound child process
	// must never compete with the request handlers (§4.2).
	runner.Register(StepKindExtractText, stepExtractText, runner.RegisterOptions{
		Pool: types.StepKindOCR, MaxAttempts: 2,
	})
	runner.Register(StepKindOCRPage, stepOCRPage, runner.RegisterOptions{
		Pool: types.StepKindOCR, Lease: 3 * time.Minute, MaxAttempts: 2,
	})
	// ocr_batch waits on the vision model, not the CPU, so it runs on the
	// llm pool next to the other network-bound steps.
	runner.Register(StepKindOCRBatch, stepOCRBatch, runner.RegisterOptions{
		Pool: types.StepKindLLM, Lease: 5 * time.Minute, MaxAttempts: 3,
	})
	// chunk_and_embed is network-bound (the embeddings gateway), so it runs
	// on the llm pool where slow-but-idle work belongs.
	runner.Register(StepKindChunkAndEmbed, stepChunkAndEmbed, runner.RegisterOptions{
		Pool: types.StepKindLLM, Lease: 3 * time.Minute, MaxAttempts: 3,
	})
}

// ---------------------------------------------------------------------------
// POST /api/dossiers/:id/pieces
// ---------------------------------------------------------------------------

// IngestPieces stores n multipart files as assets + pieces and enqueues one
// ingestion run over them.
//
// Files arrive under the `files` field (repeated) or `file`. A file that fails
// any check is reported in Rejected and the rest of the batch still lands.
func IngestPieces(c *gin.Context, dossierID string) (int, any) {
	repo, status, errBody := tenantRepo(c)
	if repo == nil {
		return status, errBody
	}
	d, status, errBody := loadDossier(repo, dossierID)
	if d == nil {
		return status, errBody
	}
	if !canAccessDossier(c, repo, *d, types.ActionWrite) {
		return http.StatusForbidden, gin.H{"detail": "Forbidden"}
	}
	user := middlewares.CurrentUser(c)

	form, err := c.MultipartForm()
	if err != nil {
		return http.StatusBadRequest, gin.H{"detail": "Invalid multipart body"}
	}
	headers := append([]*multipart.FileHeader{}, form.File["files"]...)
	headers = append(headers, form.File["file"]...)
	if len(headers) == 0 {
		return http.StatusBadRequest, gin.H{"detail": "No file supplied — use the 'files' field"}
	}
	if len(headers) > maxIngestBatchFiles {
		return http.StatusRequestEntityTooLarge, gin.H{
			"detail": fmt.Sprintf("Too many files in one upload (max %d)", maxIngestBatchFiles),
		}
	}

	// The bytes reach S3 before the rows are written, so a client disconnect
	// must not abandon the reconciliation halfway (§9: WithoutCancel is for a
	// write that reconciles an already-committed side effect).
	ctx := context.WithoutCancel(c.Request.Context())
	store := u.ForTenantID(ctx, repo.TenantID())

	var nextOrdinal int
	if err := store.Pieces().Where("dossier_id = ?", d.ID).
		Select("COALESCE(MAX(ordinal), 0)").Scan(&nextOrdinal).Error; err != nil {
		ingestLog.Errorf("Ordinal lookup failed (dossier=%s): %v", d.ID, err)
		return http.StatusBadGateway, gin.H{"detail": "Database query failed"}
	}

	resp := types.PieceUploadResponse{Accepted: []types.Piece{}, Rejected: []types.RejectedUpload{}}
	for _, h := range headers {
		piece, reason := ingestOneFile(ctx, store, *d, h, user.ID, nextOrdinal+1)
		if reason != "" {
			resp.Rejected = append(resp.Rejected, types.RejectedUpload{Name: h.Filename, Reason: reason})
			continue
		}
		nextOrdinal++
		resp.Accepted = append(resp.Accepted, *piece)
	}

	if len(resp.Accepted) > 0 {
		runID, err := enqueueIngestRun(ctx, *d, resp.Accepted, user.ID)
		if err != nil {
			// The originals are stored and the rows exist; only the run
			// failed to enqueue. Say so rather than pretending the upload
			// failed — the pieces are re-ingestable from the dossier.
			ingestLog.Errorf("Ingestion run enqueue failed (dossier=%s pieces=%d): %v", d.ID, len(resp.Accepted), err)
		} else {
			resp.RunID = runID
		}
	}

	audit(store, user.ID, "piece.ingest", "dossier", d.ID, jsonbOf(map[string]any{
		"accepted": len(resp.Accepted), "rejected": len(resp.Rejected), "run_id": resp.RunID,
	}))
	// Shapes only: counts, never a filename or a document excerpt (§7.4).
	ingestLog.Infof("Ingested upload (dossier=%s tenant=%s accepted=%d rejected=%d run=%s)",
		d.ID, store.TenantID(), len(resp.Accepted), len(resp.Rejected), resp.RunID)
	return http.StatusCreated, resp
}

// ingestOneFile runs the whole per-file pipeline. It returns either the
// created piece or a human-readable rejection reason — never both, never
// neither.
func ingestOneFile(ctx context.Context, repo *u.TenantRepo, d types.Dossier,
	h *multipart.FileHeader, userID string, ordinal int) (*types.Piece, string) {

	if h.Size > maxIngestFileBytes {
		return nil, fmt.Sprintf("File is larger than the %d MB limit", maxIngestFileBytes>>20)
	}
	src, err := h.Open()
	if err != nil {
		ingestLog.Warnf("Could not open an uploaded part (size=%d): %v", h.Size, err)
		return nil, "Could not read the uploaded file"
	}
	defer src.Close()

	// Read once, with a hard ceiling: everything below (hash, sniff, zip
	// inspection) works on the same bytes, and nothing streams an unbounded
	// body into memory.
	raw, err := io.ReadAll(io.LimitReader(src, maxIngestFileBytes+1))
	if err != nil {
		ingestLog.Warnf("Could not read an uploaded part (size=%d): %v", h.Size, err)
		return nil, "Could not read the uploaded file"
	}
	if len(raw) > maxIngestFileBytes {
		return nil, fmt.Sprintf("File is larger than the %d MB limit", maxIngestFileBytes>>20)
	}
	if len(raw) == 0 {
		return nil, "File is empty"
	}

	// HASH BEFORE PARSING (§7.1): the recorded digest is of the bytes the
	// lawyer handed us, not of anything we touched afterwards.
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	// SNIFF THE MAGIC BYTES, NEVER THE EXTENSION. A .pdf that is really a zip
	// is caught here, before any parser sees it.
	kind, mime, reason := sniffDocument(raw)
	if reason != "" {
		ingestLog.Warnf("Upload rejected at sniff (bytes=%d sha=%s…): %s", len(raw), digest[:8], reason)
		return nil, reason
	}

	if reason := validateDocument(kind, raw); reason != "" {
		ingestLog.Warnf("Upload rejected at validation (kind=%s bytes=%d sha=%s…): %s",
			kind, len(raw), digest[:8], reason)
		return nil, reason
	}

	// Re-ingesting the same bytes into the same dossier is a no-op the lawyer
	// should hear about, not a second piece silently competing for an ordinal.
	if dup, err := ingestDuplicate(repo, d.ID, digest); err != nil {
		return nil, "Database query failed"
	} else if dup {
		return nil, "This document is already in the dossier (identical SHA-256)"
	}

	// <app>/<tenant>/<dossier>/<uuid>-<name>, with the hard <app>/ prefix
	// guard inside DossierKey. The stored object is byte-for-byte the upload.
	key := u.DossierKey(repo.TenantID(), d.ID, uuid.NewString()+"-"+path.Base(h.Filename))
	if err := u.Blob.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), mime); err != nil {
		ingestLog.Errorf("Original store failed (dossier=%s bytes=%d): %v", d.ID, len(raw), err)
		return nil, "Could not store the document"
	}

	asset := types.Asset{
		ID:        uuid.NewString(),
		S3Key:     key,
		Kind:      types.AssetKindOriginal,
		Mime:      mime,
		Size:      int64(len(raw)),
		SHA256:    digest,
		CreatedBy: userID,
	}
	piece := types.Piece{
		ID:                  uuid.NewString(),
		DossierID:           d.ID,
		SourceAssetID:       asset.ID,
		OriginalName:        h.Filename,
		Ordinal:             ordinal,
		ClassificationState: types.PieceStatePending,
		DatePrecision:       types.DatePrecisionUnknown,
	}
	if err := repo.Tx(func(tx *u.TenantRepo) error {
		if err := tx.Create(&asset); err != nil {
			return err
		}
		return tx.Create(&piece)
	}); err != nil {
		// The object is already in the store; drop it so a failed row write
		// cannot leave an unreferenced original behind.
		if rmErr := u.Blob.Delete(ctx, key); rmErr != nil {
			ingestLog.Errorf("Orphaned original cleanup failed (dossier=%s): %v", d.ID, rmErr)
		}
		ingestLog.Errorf("Asset/piece insert failed (dossier=%s): %v", d.ID, err)
		return nil, "Database insert failed"
	}

	ingestLog.Infof("Stored an original (piece=%s dossier=%s kind=%s bytes=%d)",
		piece.ID, d.ID, kind, len(raw))
	return &piece, ""
}

// ingestDuplicate reports whether this dossier already holds a piece whose
// original has the same digest. Two tenant-scoped queries rather than a join,
// because both tables carry tenant_id and an unqualified predicate would be
// ambiguous.
func ingestDuplicate(repo *u.TenantRepo, dossierID, digest string) (bool, error) {
	var assetIDs []string
	if err := repo.Assets().Where("sha256 = ?", digest).Pluck("id", &assetIDs).Error; err != nil {
		ingestLog.Errorf("Duplicate lookup failed (dossier=%s): %v", dossierID, err)
		return false, err
	}
	if len(assetIDs) == 0 {
		return false, nil
	}
	var n int64
	if err := repo.Pieces().Where("dossier_id = ? AND source_asset_id IN ?", dossierID, assetIDs).
		Count(&n).Error; err != nil {
		ingestLog.Errorf("Duplicate count failed (dossier=%s): %v", dossierID, err)
		return false, err
	}
	return n > 0, nil
}

// enqueueIngestRun commits one run with an extract_text and a qualify_piece
// step per piece. The rest of the plan grows from there: extract_text decides
// whether the document needs OCR and enqueues the follow-up steps itself (§4.1
// — the fan-out width is data, the shape is code).
//
// Qualification runs at INGESTION, not only inside the full
// dossier_organize_v1 mission, because the pièces table is editable from the
// moment the upload lands: a lawyer opening it should find the type, the date
// and the titre already proposed rather than fifteen empty rows. Nothing here
// numbers or renames anything — the ordinal and the nom normalisé stay the
// classement's job (§7.1), and classification_state stops at `proposed`.
func enqueueIngestRun(ctx context.Context, d types.Dossier, pieces []types.Piece, userID string) (string, error) {
	steps := make([]runner.StepSpec, 0, len(pieces)*2)
	for _, p := range pieces {
		extractKey := StepKindExtractText + ":" + p.ID
		qualifyKey := StepKindQualifyPiece + ":" + p.ID
		steps = append(steps, runner.StepSpec{
			Key:   extractKey,
			Kind:  StepKindExtractText,
			Label: "Extraction du texte",
			Input: ingestStepInput{
				PieceID: p.ID, AssetID: p.SourceAssetID,
				// qualify_piece reads the text from piece_chunks, which
				// chunk_and_embed writes — and for a scan that step is itself
				// behind the whole OCR fan-out. Neither exists yet, so the
				// edge cannot be drawn here: extract_text draws it, from this
				// list, once it has measured the document.
				Dependents: []string{qualifyKey},
			},
		})
		steps = append(steps, runner.StepSpec{
			Key:       qualifyKey,
			Kind:      StepKindQualifyPiece,
			Label:     "Qualification",
			DependsOn: []string{extractKey},
			Input:     QualifyPieceInput{PieceID: p.ID},
		})
	}
	run, err := runner.Enqueue(ctx, runner.RunSpec{
		TenantID:        d.TenantID,
		DossierID:       d.ID,
		RequestedBy:     userID,
		WorkflowType:    types.WorkflowDossierOrganizeV1,
		WorkflowVersion: 1,
		Instruction:     "Ingestion des pièces",
		Steps:           steps,
	})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

// ---------------------------------------------------------------------------
// Magic-byte sniffing and per-format validation
// ---------------------------------------------------------------------------

// Document kinds this slice accepts.
const (
	docKindPDF  = "pdf"
	docKindDOCX = "docx"
	docKindXLSX = "xlsx"
)

// sniffDocument identifies a file from its leading bytes and returns
// (kind, mime, ""). A file we will not process comes back with a reason.
//
// The filename plays no part in this. It cannot: the extension is attacker
// (or merely careless) controlled, and a .pdf that is really a zip must not
// reach a PDF parser.
func sniffDocument(b []byte) (kind, mime, reason string) {
	switch {
	case pdfHeaderOffset(b) >= 0:
		return docKindPDF, "application/pdf", ""
	case bytes.HasPrefix(b, []byte("PK\x03\x04")),
		bytes.HasPrefix(b, []byte("PK\x05\x06")),
		bytes.HasPrefix(b, []byte("PK\x07\x08")):
		return sniffZipPackage(b)
	}
	return "", "", "Unsupported file type — this dossier accepts PDF, DOCX and XLSX"
}

// pdfHeaderOffset finds the %PDF- header. The spec tolerates leading bytes
// before it, and poppler accepts a header inside the first 1 KB, so we look in
// the same window rather than demanding offset 0.
func pdfHeaderOffset(b []byte) int {
	window := b
	if len(window) > 1024 {
		window = window[:1024]
	}
	return bytes.Index(window, []byte("%PDF-"))
}

// sniffZipPackage classifies an OOXML package by the parts it contains, not by
// its extension.
func sniffZipPackage(b []byte) (kind, mime, reason string) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", "", "File looks like a zip archive but could not be opened"
	}
	var hasDoc, hasSheet bool
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			hasDoc = true
		case "xl/workbook.xml":
			hasSheet = true
		}
	}
	switch {
	case hasDoc:
		return docKindDOCX, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ""
	case hasSheet:
		return docKindXLSX, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ""
	}
	return "", "", "Zip archive is neither a DOCX nor an XLSX document"
}

// validateDocument runs the format-specific refusals: encrypted PDFs, launch
// actions, zip bombs and macros.
func validateDocument(kind string, b []byte) string {
	switch kind {
	case docKindPDF:
		return validatePDF(b)
	case docKindDOCX, docKindXLSX:
		return validateOOXML(b)
	}
	return "Unsupported file type"
}

// validatePDF refuses what we cannot or must not process.
//
// Encryption: an encrypted PDF is REPORTED, never silently failed downstream
// (§7.1). /Encrypt in a trailer dictionary is how the standard security
// handler announces itself; we match the token anywhere in the file, which is
// deliberately conservative — a false positive is a clear message asking for
// an unprotected copy, a false negative is pdftotext returning nothing and
// nobody knowing why.
//
// Active content: /Launch runs an external application and has no legitimate
// place in an exhibit, so it is refused. /JavaScript is logged, not refused —
// embedded scripts are common in official French forms, and nothing in this
// pipeline executes a PDF: poppler renders and extracts, it does not
// interpret.
func validatePDF(b []byte) string {
	if bytes.Contains(b, []byte("/Encrypt")) {
		return "Encrypted or password-protected PDF — supply an unprotected copy"
	}
	if bytes.Contains(b, []byte("/Launch")) {
		return "PDF contains a launch action (active content) and was refused"
	}
	if bytes.Contains(b, []byte("/JavaScript")) || bytes.Contains(b, []byte("/JS")) {
		// A shape, not content: the count of markers, never the script.
		ingestLog.Warnf("PDF carries JavaScript markers — accepted, never executed (bytes=%d)", len(b))
	}
	return ""
}

// validateOOXML bounds a docx/xlsx package before anything reads it.
//
// A zip is a decompression bomb waiting to happen: entry sizes are declared by
// the archive itself and a 40 KB file can announce 40 GB. We cap the entry
// count, the declared total, and the per-entry compression ratio — and we do
// it on the HEADERS, so nothing is inflated to find out.
//
// Macros are refused outright. We never execute an office document; refusing
// vbaProject.bin means we do not store one either.
func validateOOXML(b []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "Document package could not be opened"
	}
	if len(zr.File) > maxIngestZipEntries {
		return "Document package contains too many parts"
	}
	var total uint64
	for _, f := range zr.File {
		name := f.Name
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return "Document package contains an unsafe entry path"
		}
		if strings.Contains(strings.ToLower(name), "vbaproject") ||
			strings.HasSuffix(strings.ToLower(name), ".vba") {
			return "Document contains macros (active content) and was refused"
		}
		total += f.UncompressedSize64
		if total > maxIngestZipBytes {
			return "Document package expands beyond the allowed size (decompression bomb)"
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > maxIngestZipRatio &&
			f.UncompressedSize64 > 1<<20 {
			return "Document package has an implausible compression ratio (decompression bomb)"
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Step: extract_text
// ---------------------------------------------------------------------------

// extractOutput is the step's jsonb output. Shapes only.
type extractOutput struct {
	Pages        int    `json:"pages"`
	Chars        int    `json:"chars"`
	CharsPerPage int    `json:"chars_per_page"`
	Scanned      bool   `json:"scanned"`
	OCRPages     int    `json:"ocr_pages,omitempty"`
	Engine       string `json:"engine"`
}

// stepExtractText pulls the text layer out of the original.
//
// PDFs go through `pdftotext -layout` (poppler), invoked with an argument
// array through utils.RunTool — never a shell string built from a filename.
// DOCX/XLSX are parsed as zip+XML in pure Go: no external tool, and the
// document is never executed.
//
// When a PDF carries fewer than ~150 characters per page it is a SCAN, and the
// step fans out the OCR (§7.2) — one ocr_page step per page, or one ocr_batch
// step per page range in vision mode — with a chunk_and_embed step waiting
// behind them. A text-layer document goes straight to chunking.
func stepExtractText(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in ingestStepInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	piece, asset, err := loadIngestTargets(repo, in)
	if err != nil {
		return nil, err
	}

	raw, err := ingestFetch(ctx, asset.S3Key)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	out, err := extractPiece(ctx, repo, *piece, asset.Mime, raw)
	if err != nil {
		return nil, err
	}
	pages, chars, perPage, scanned, engine := out.Pages, out.Chars, out.CharsPerPage, out.Scanned, out.Engine
	ocrPages := out.OCRPages

	// Extraction advances a piece; it never demotes one. Re-extracting a piece
	// that is already qualified or reviewed — a re-ingestion, a replayed
	// attempt — re-reads its text but must not undo the lawyer's review.
	if err := repo.Pieces().Where("id = ?", piece.ID).Updates(map[string]any{
		"page_count": pages,
		"text_chars": chars,
		"classification_state": gorm.Expr("CASE WHEN classification_state IN (?, ?) THEN ? ELSE classification_state END",
			types.PieceStatePending, types.PieceStateFailed, types.PieceStateExtracted),
	}).Error; err != nil {
		return nil, runner.Retry("piece_update_failed", "could not record the extraction result")
	}

	// The follow-up plan. Bounded: a 900-page scan does not become 900 OCR
	// steps, it becomes the first maxOCRPages and a recorded truncation.
	var follow []runner.StepSpec
	chunkStep := runner.StepSpec{
		Key:   StepKindChunkAndEmbed + ":" + piece.ID,
		Kind:  StepKindChunkAndEmbed,
		Label: "Découpage et vectorisation",
		Input: ingestStepInput{PieceID: piece.ID, AssetID: asset.ID, PageCount: pages, Scanned: scanned},
	}
	if scanned {
		if ocrPages == 0 {
			return nil, runner.Permanent("empty_document", "the document has no page to read")
		}
		if pages > ocrPages {
			ingestLog.Warnf("Scan longer than the OCR cap — reading the first %d of %d pages (piece=%s)",
				ocrPages, pages, piece.ID)
		}
		chunkStep.Input = ingestStepInput{PieceID: piece.ID, AssetID: asset.ID, PageCount: ocrPages, Scanned: true}
		for _, spec := range ocrFanOut(piece.ID, asset.ID, ocrPages) {
			follow = append(follow, spec)
			chunkStep.DependsOn = append(chunkStep.DependsOn, spec.Key)
		}
	}
	follow = append(follow, chunkStep)
	// A second attempt of this step must not try to re-create the steps the
	// first one already committed: (run_id, step_key) is unique, and a
	// replayed attempt has to CONVERGE rather than fail (§4.3).
	if pruned := pruneExistingSteps(repo, sc.Run.ID, follow); len(pruned) > 0 {
		if _, err := runner.EnqueueSteps(ctx, sc.Run.ID, pruned); err != nil {
			return nil, runner.Retry("enqueue_failed", "could not enqueue the follow-up ingestion steps")
		}
	}

	// The piece's text is READABLE only once chunk_and_embed has written
	// piece_chunks — that is what qualify_piece reads, and for a scan the
	// chunk step is itself behind the whole OCR fan-out. The plan cannot draw
	// that edge: chunk_and_embed does not exist until this step enqueues it,
	// and whether there is an OCR fan-out in front of it is not known until
	// the text layer has been measured. So this step draws it, and it draws
	// it BEFORE returning — the edges commit while this step is still
	// running, so a dependent is never claimable in the window between the
	// two writes.
	if len(in.Dependents) > 0 {
		for _, dep := range in.Dependents {
			if err := runner.DependOn(ctx, sc.Run.ID, dep, []string{chunkStep.Key}); err != nil {
				return nil, runner.Retry("depend_failed", "could not make the downstream steps wait for the text")
			}
		}
	}

	ingestLog.Infof("Extracted text (piece=%s pages=%d chars=%d chars_per_page=%d scanned=%t engine=%s %s)",
		piece.ID, pages, chars, perPage, scanned, engine, time.Since(started).Round(time.Millisecond))
	return out, nil
}

// extractPiece extracts an original and stores its canonical text and
// extraction record — everything extract_text does before planning what
// follows. A scan stores no text (OCR writes it page by page) but its record
// keeps the page count and the OCR cap.
func extractPiece(ctx context.Context, repo *u.TenantRepo, piece types.Piece, mime string, raw []byte) (extractOutput, error) {
	var ex extraction
	var err error
	engine := "pdftotext"
	switch mime {
	case "application/pdf":
		ex.Pages, err = pdfTextPages(ctx, raw)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		engine = "ooxml"
		ex, err = docxExtract(raw)
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		engine = "ooxml"
		ex, err = xlsxExtract(raw)
	default:
		return extractOutput{}, runner.Permanent("unsupported_mime", "no text extractor for this document type")
	}
	if err != nil {
		return extractOutput{}, err
	}
	pages := ex.Pages
	chars := ingestCharCount(pages)
	out := extractOutput{Pages: len(pages), Chars: chars, Engine: engine}
	if len(pages) > 0 {
		out.CharsPerPage = chars / len(pages)
	}
	out.Scanned = mime == "application/pdf" && out.CharsPerPage < scannedCharsPerPage
	if out.Scanned {
		out.OCRPages = min(len(pages), maxOCRPages)
	}

	// The text first, then the record that indexes it: a record names the
	// text it was cut from (text_sha256), so a crash between the two writes
	// leaves a mismatch the reader detects, never a wrong offset.
	if mime == "application/pdf" {
		ex.Record = pdfExtractionRecord(pages, out.Scanned, out.OCRPages)
	}
	if !out.Scanned {
		text := ingestJoinPages(pages)
		if err := ingestPutText(ctx, repo, piece, textSidecarKey(repo.TenantID(), piece.DossierID, piece.ID), text); err != nil {
			return extractOutput{}, err
		}
		ex.Record.TextSHA256 = sha256Hex(text)
	}
	if err := ingestPutRecord(ctx, repo, piece, ex.Record); err != nil {
		return extractOutput{}, err
	}
	return out, nil
}

// pruneExistingSteps drops the specs whose step_key is already in the run, so
// a retried fan-out is a no-op instead of a unique-constraint failure. The
// dependency keys are left untouched: Enqueue resolves them against the steps
// already in the run.
func pruneExistingSteps(repo *u.TenantRepo, runID string, specs []runner.StepSpec) []runner.StepSpec {
	keys := make([]string, 0, len(specs))
	for _, s := range specs {
		keys = append(keys, s.Key)
	}
	var existing []string
	if err := repo.RunSteps().Where("run_id = ? AND step_key IN ?", runID, keys).
		Pluck("step_key", &existing).Error; err != nil {
		ingestLog.Warnf("Follow-up step lookup failed (run=%s): %v", runID, err)
		return specs
	}
	if len(existing) == 0 {
		return specs
	}
	seen := make(map[string]bool, len(existing))
	for _, k := range existing {
		seen[k] = true
	}
	out := specs[:0]
	for _, s := range specs {
		if !seen[s.Key] {
			out = append(out, s)
		}
	}
	ingestLog.Infof("Follow-up steps already present (run=%s skipped=%d)", runID, len(existing))
	return out
}

// ---------------------------------------------------------------------------
// Text extractors
// ---------------------------------------------------------------------------

// ingestPage is one page of extracted text, so a chunk can carry the page
// range a lawyer needs to check the source.
type ingestPage struct {
	Number int
	Text   string
}

// pdfTextPages runs `pdftotext -layout` and splits on the form feeds poppler
// writes between pages.
func pdfTextPages(ctx context.Context, raw []byte) ([]ingestPage, error) {
	dir, cleanup, err := u.JobDir("extract")
	if err != nil {
		return nil, runner.Retry("jobdir_failed", "could not create a work directory")
	}
	defer cleanup()

	src := path.Join(dir, "in.pdf")
	if err := ingestWriteFile(src, raw); err != nil {
		return nil, runner.Retry("jobdir_failed", "could not stage the document")
	}

	stdout, err := u.RunTool(ctx, u.ToolSpec{
		Name:       "pdftotext",
		Args:       []string{"-layout", "-enc", "UTF-8", "-q", src, "-"},
		Dir:        dir,
		InputPaths: []string{src},
		Timeout:    2 * time.Minute,
	})
	if err != nil {
		return nil, ingestToolError("pdftotext", err)
	}
	return splitFormFeeds(string(stdout)), nil
}

// splitFormFeeds turns poppler's \f-separated output into pages.
func splitFormFeeds(s string) []ingestPage {
	parts := strings.Split(s, "\f")
	// A trailing form feed produces one empty tail part; poppler always
	// writes one, so drop it rather than inventing a blank page.
	if len(parts) > 1 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	pages := make([]ingestPage, 0, len(parts))
	for i, p := range parts {
		pages = append(pages, ingestPage{Number: i + 1, Text: p})
	}
	return pages
}

// docxTextPages extracts a DOCX for the callers that only want its text (a
// model act): the body paragraphs of the canonical extraction, one blank line
// apart. A DOCX has no page structure until it is laid out, so it is one
// logical unit — never a page anyone can cite.
func docxTextPages(raw []byte) ([]ingestPage, error) {
	ex, err := docxExtract(raw)
	if err != nil {
		return nil, err
	}
	return ex.Pages, nil
}

// docxExtract reads the body paragraphs of word/document.xml in pure Go and
// records where each one sits in the sidecar text (PieceTextVersion rules,
// piece_locators.go). Notes, comments, headers and footers are not body text:
// they are counted as limitations rather than silently dropped.
func docxExtract(raw []byte) (extraction, error) {
	part, err := ooxmlPart(raw, "word/document.xml")
	if errors.Is(err, errOOXMLPartMissing) {
		return extraction{}, runner.Permanent("docx_parse_failed", "the document has no body part")
	}
	if err != nil {
		return extraction{}, err
	}
	paras, listItems, err := docxParagraphs(part)
	if err != nil {
		return extraction{}, runner.Permanent("docx_parse_failed", "the document XML could not be parsed")
	}
	rec := extractionRecord{Version: PieceTextVersion, Format: pieceFormatDOCX}
	var b strings.Builder
	pos := 0
	for i, p := range paras {
		if i > 0 {
			b.WriteString("\n\n")
			pos += 2
		}
		n := utf8.RuneCountInString(p)
		rec.Units = append(rec.Units, extractionUnit{Page: 1, Start: pos, End: pos + n, Paragraph: i + 1})
		b.WriteString(p)
		pos += n
	}
	if listItems > 0 {
		rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitDOCXListLabels, Count: listItems})
	}
	rec.Limitations = append(rec.Limitations, docxOtherParts(raw)...)
	return extraction{Pages: []ingestPage{{Number: 1, Text: b.String()}}, Record: rec}, nil
}

// docxParagraphs returns the text of every non-empty paragraph, in document
// order (table cells and text boxes included), and how many of them carry an
// automatic list number — a label Word draws that is not in the text.
//
// Inside a paragraph: w:t text, w:tab → \t, w:br / w:cr → \n, w:noBreakHyphen
// → U+2011. Deleted text (w:delText), field codes (w:instrText) and the
// legacy copy of an mc:AlternateContent (mc:Fallback) are not text.
func docxParagraphs(part []byte) ([]string, int, error) {
	dec := xml.NewDecoder(bytes.NewReader(part))
	// Entities only, no external references: an office document must never
	// make this process fetch anything.
	dec.Strict = false
	type openPara struct {
		b        strings.Builder
		numbered bool
	}
	var (
		open     []*openPara
		out      []string
		listed   int
		fallback int
		runs     int
		inText   bool
	)
	top := func() *openPara {
		if len(open) == 0 {
			return nil
		}
		return open[len(open)-1]
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "Fallback" {
				fallback++
				continue
			}
			if fallback > 0 {
				continue
			}
			p := top()
			switch t.Name.Local {
			case "p":
				open = append(open, &openPara{})
			case "r":
				runs++
			case "t":
				inText = p != nil
			case "numPr":
				if p != nil {
					p.numbered = true
				}
			case "tab":
				if p != nil && runs > 0 {
					p.b.WriteByte('\t')
				}
			case "br", "cr":
				if p != nil && runs > 0 {
					p.b.WriteByte('\n')
				}
			case "noBreakHyphen":
				if p != nil && runs > 0 {
					p.b.WriteRune('‑')
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Fallback" {
				if fallback > 0 {
					fallback--
				}
				continue
			}
			if fallback > 0 {
				continue
			}
			switch t.Name.Local {
			case "p":
				if p := top(); p != nil {
					open = open[:len(open)-1]
					if text := sidecarSafe(p.b.String()); strings.TrimSpace(text) != "" {
						out = append(out, text)
						if p.numbered {
							listed++
						}
					}
				}
			case "r":
				if runs > 0 {
					runs--
				}
			case "t":
				inText = false
			}
		case xml.CharData:
			if inText && fallback == 0 {
				if p := top(); p != nil {
					p.b.Write(t)
				}
			}
		}
	}
	return out, listed, nil
}

// docxOtherParts counts the text the body extraction leaves out: footnotes and
// endnotes, comments, headers and footers. A part that holds nothing but the
// separators Word always writes counts for nothing.
func docxOtherParts(raw []byte) []ExtractionLimitation {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	for _, f := range zr.File {
		code := ""
		switch {
		case f.Name == "word/footnotes.xml", f.Name == "word/endnotes.xml":
			code = LimitDOCXNotes
		case f.Name == "word/comments.xml":
			code = LimitDOCXComments
		case strings.HasPrefix(f.Name, "word/header") && strings.HasSuffix(f.Name, ".xml"),
			strings.HasPrefix(f.Name, "word/footer") && strings.HasSuffix(f.Name, ".xml"):
			code = LimitDOCXHeadersFooters
		default:
			continue
		}
		part, err := ooxmlPart(raw, f.Name)
		if err != nil {
			continue
		}
		if paras, _, err := docxParagraphs(part); err == nil {
			counts[code] += len(paras)
		}
	}
	var out []ExtractionLimitation
	for _, code := range []string{LimitDOCXNotes, LimitDOCXComments, LimitDOCXHeadersFooters} {
		if counts[code] > 0 {
			out = append(out, ExtractionLimitation{Code: code, Count: counts[code]})
		}
	}
	return out
}

// sidecarSafe keeps the form feed — the sidecar's page separator — out of
// extracted office text, where XML forbids it anyway.
func sidecarSafe(s string) string {
	return strings.ReplaceAll(s, "\f", " ")
}

// ---------------------------------------------------------------------------
// XLSX
// ---------------------------------------------------------------------------

// maxXLSXCells bounds one workbook's extraction. Beyond it the rest is
// reported (xlsx_cell_limit), never silently absent.
const maxXLSXCells = 200_000

// xlsxExtract reads every worksheet, in workbook order, under its real name:
// one sidecar page per worksheet, rows one per line, cells joined by tabs,
// and every non-empty cell recorded by reference with its exact offsets
// (PieceTextVersion rules, piece_locators.go).
func xlsxExtract(raw []byte) (extraction, error) {
	book, err := xlsxReadWorkbook(raw)
	if err != nil {
		return extraction{}, err
	}
	shared, err := xlsxSharedStrings(raw)
	if err != nil {
		return extraction{}, err
	}
	styles := xlsxReadStyles(raw)

	rec := extractionRecord{Version: PieceTextVersion, Format: pieceFormatXLSX}
	var pages []ingestPage
	cells, noValue, others, skipped := 0, 0, 0, 0
	for _, sh := range book.sheets {
		if !sh.worksheet {
			others++
			continue
		}
		if rec.Truncated {
			skipped++
			continue
		}
		part, err := ooxmlPart(raw, sh.part)
		if errors.Is(err, errOOXMLPartMissing) || sh.part == "" {
			rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitXLSXSheetMissing, Detail: sh.name})
			continue
		}
		if err != nil {
			return extraction{}, err
		}
		got, nv, truncated, err := xlsxSheetCells(part, shared, styles, book.date1904, maxXLSXCells-cells)
		if err != nil {
			return extraction{}, err
		}
		cells += len(got)
		noValue += nv
		rec.Sheets = append(rec.Sheets, sh.name)
		k := len(rec.Sheets)
		var b strings.Builder
		pos, row := 0, 0
		for i, c := range got {
			if i > 0 {
				sep := byte('\t')
				if c.row != row {
					sep = '\n'
				}
				b.WriteByte(sep)
				pos++
			}
			row = c.row
			n := utf8.RuneCountInString(c.text)
			rec.Units = append(rec.Units, extractionUnit{Page: k, Start: pos, End: pos + n, Sheet: k, Cell: c.ref})
			b.WriteString(c.text)
			pos += n
		}
		pages = append(pages, ingestPage{Number: k, Text: b.String()})
		if sh.state == "hidden" || sh.state == "veryHidden" {
			rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitXLSXHiddenSheet, Detail: sh.name})
		}
		if truncated {
			rec.Truncated = true
		}
	}
	if rec.Truncated {
		rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitXLSXCellLimit, Count: maxXLSXCells, Detail: strconv.Itoa(skipped)})
	}
	if noValue > 0 {
		rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitXLSXFormulaNoValue, Count: noValue})
	}
	if others > 0 {
		rec.Limitations = append(rec.Limitations, ExtractionLimitation{Code: LimitXLSXOtherSheets, Count: others})
	}
	return extraction{Pages: pages, Record: rec}, nil
}

type xlsxSheetRef struct {
	name, state, part string
	worksheet         bool
}

type xlsxBook struct {
	sheets   []xlsxSheetRef
	date1904 bool
}

// xlsxReadWorkbook lists the sheets in workbook (tab) order with the package
// part each one lives in — never the part names' alphabetical order.
func xlsxReadWorkbook(raw []byte) (xlsxBook, error) {
	var book xlsxBook
	part, err := ooxmlPart(raw, "xl/workbook.xml")
	if errors.Is(err, errOOXMLPartMissing) {
		return book, runner.Permanent("xlsx_parse_failed", "the workbook part is missing")
	}
	if err != nil {
		return book, err
	}
	type rel struct{ target, kind string }
	rels := map[string]rel{}
	if relsPart, err := ooxmlPart(raw, "xl/_rels/workbook.xml.rels"); err == nil {
		dec := xml.NewDecoder(bytes.NewReader(relsPart))
		dec.Strict = false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if t, ok := tok.(xml.StartElement); ok && t.Name.Local == "Relationship" {
				var id, target, kind, mode string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "Id":
						id = a.Value
					case "Target":
						target = a.Value
					case "Type":
						kind = a.Value
					case "TargetMode":
						mode = a.Value
					}
				}
				if id == "" || target == "" || mode == "External" {
					continue
				}
				if strings.HasPrefix(target, "/") {
					target = path.Clean(strings.TrimPrefix(target, "/"))
				} else {
					target = path.Join("xl", target)
				}
				rels[id] = rel{target: target, kind: kind}
			}
		}
	}

	dec := xml.NewDecoder(bytes.NewReader(part))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return book, runner.Permanent("xlsx_parse_failed", "the workbook part could not be parsed")
		}
		t, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch t.Name.Local {
		case "workbookPr":
			for _, a := range t.Attr {
				if a.Name.Local == "date1904" {
					book.date1904 = a.Value == "1" || strings.EqualFold(a.Value, "true")
				}
			}
		case "sheet":
			var ref xlsxSheetRef
			var id string
			for _, a := range t.Attr {
				switch {
				case a.Name.Local == "name":
					ref.name = a.Value
				case a.Name.Local == "state":
					ref.state = a.Value
				case a.Name.Local == "id" && strings.Contains(a.Name.Space, "relationships"):
					id = a.Value
				}
			}
			if r, ok := rels[id]; ok {
				ref.part = r.target
				ref.worksheet = strings.HasSuffix(r.kind, "/worksheet")
			} else {
				// No relationship: a broken package. Report the sheet as
				// missing rather than guess which part it was.
				ref.worksheet = true
			}
			book.sheets = append(book.sheets, ref)
		}
	}
	return book, nil
}

// xlsxSharedStrings reads the shared string table, which is where most cell
// text actually lives. A workbook without one is legal. Phonetic guide runs
// (rPh) are reading aids, not text.
func xlsxSharedStrings(raw []byte) ([]string, error) {
	part, err := ooxmlPart(raw, "xl/sharedStrings.xml")
	if err != nil {
		if errors.Is(err, errOOXMLPartMissing) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	var cur strings.Builder
	var inT bool
	phonetic := 0
	dec := xml.NewDecoder(bytes.NewReader(part))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, runner.Permanent("xlsx_parse_failed", "the shared string table could not be parsed")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				cur.Reset()
			case "rPh":
				phonetic++
			case "t":
				inT = phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, xlsxUnescape(cur.String()))
			case "rPh":
				if phonetic > 0 {
					phonetic--
				}
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
	return out, nil
}

// xlsxStyles is what the canonical text needs from styles.xml: each cell
// style's number format.
type xlsxStyles struct {
	xf    []int          // cellXfs index → numFmtId
	codes map[int]string // custom number format codes
}

// xlsxReadStyles reads the number formats. A workbook without styles (or with
// unreadable ones) keeps every number in its stored form.
func xlsxReadStyles(raw []byte) xlsxStyles {
	st := xlsxStyles{codes: map[int]string{}}
	part, err := ooxmlPart(raw, "xl/styles.xml")
	if err != nil {
		return st
	}
	dec := xml.NewDecoder(bytes.NewReader(part))
	dec.Strict = false
	inXfs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "numFmt":
				id, code := -1, ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "numFmtId":
						id, _ = strconv.Atoi(a.Value)
					case "formatCode":
						code = a.Value
					}
				}
				if id >= 0 {
					st.codes[id] = code
				}
			case "cellXfs":
				inXfs = true
			case "xf":
				if inXfs {
					id := 0
					for _, a := range t.Attr {
						if a.Name.Local == "numFmtId" {
							id, _ = strconv.Atoi(a.Value)
						}
					}
					st.xf = append(st.xf, id)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
	}
	return st
}

// xlsxCell is one non-empty cell of a worksheet.
type xlsxCell struct {
	ref  string // "B4"
	row  int
	text string
}

// xlsxSheetCells walks one worksheet's cells in sheet order. room bounds how
// many may still be read; truncated says the bound was hit. noValue counts
// formula cells saved without a computed value — their result is not in the
// file, so there is no text to read.
func xlsxSheetCells(part []byte, shared []string, st xlsxStyles, date1904 bool, room int) (cells []xlsxCell, noValue int, truncated bool, err error) {
	dec := xml.NewDecoder(bytes.NewReader(part))
	dec.Strict = false
	var (
		row, col          int
		inCell, inV, inIS bool
		inT, hasFormula   bool
		phonetic, style   int
		cellType, ref     string
		value, inline     strings.Builder
	)
	for {
		tok, terr := dec.Token()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			return nil, 0, false, runner.Permanent("xlsx_parse_failed", "a worksheet could not be parsed")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				next := row + 1
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						if n, err := strconv.Atoi(a.Value); err == nil && n > 0 {
							next = n
						}
					}
				}
				row, col = next, 0
			case "c":
				inCell, hasFormula = true, false
				cellType, ref, style = "", "", 0
				value.Reset()
				inline.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						ref = a.Value
					case "t":
						cellType = a.Value
					case "s":
						style, _ = strconv.Atoi(a.Value)
					}
				}
				if c, r, ok := parseCellRef(ref); ok {
					col = c
					if r > 0 {
						row = r
					}
				} else {
					col++
				}
				ref = columnName(col) + strconv.Itoa(row)
			case "v":
				inV = inCell
			case "f":
				hasFormula = hasFormula || inCell
			case "is":
				inIS = inCell
			case "rPh":
				phonetic++
			case "t":
				inT = inIS && phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "rPh":
				if phonetic > 0 {
					phonetic--
				}
			case "t":
				inT = false
			case "c":
				if !inCell {
					continue
				}
				inCell = false
				text := xlsxCellText(cellType, value.String(), inline.String(), style, shared, st, date1904)
				if text == "" {
					if hasFormula && strings.TrimSpace(value.String()) == "" {
						noValue++
					}
					continue
				}
				if len(cells) >= room {
					return cells, noValue, true, nil
				}
				cells = append(cells, xlsxCell{ref: ref, row: row, text: sidecarSafe(text)})
			}
		case xml.CharData:
			switch {
			case inV:
				value.Write(t)
			case inT:
				inline.Write(t)
			}
		}
	}
	return cells, noValue, false, nil
}

// xlsxCellText is a cell's canonical text (PieceTextVersion): strings as
// written, TRUE/FALSE for booleans, error codes verbatim, numbers in their
// stored decimal form — an ISO 8601 date when the number format is a date or
// time, the stored form plus the ISO currency when the format shows € or £.
func xlsxCellText(cellType, v, inline string, style int, shared []string, st xlsxStyles, date1904 bool) string {
	switch cellType {
	case "s":
		if idx, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && idx >= 0 && idx < len(shared) {
			return shared[idx]
		}
		return ""
	case "inlineStr":
		return xlsxUnescape(inline)
	case "str", "e":
		return xlsxUnescape(v)
	case "b":
		switch strings.TrimSpace(v) {
		case "1":
			return "TRUE"
		case "0":
			return "FALSE"
		}
		return strings.TrimSpace(v)
	case "d":
		return strings.TrimSpace(v)
	}
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	return st.numberText(s, style, date1904)
}

func (st xlsxStyles) numberText(v string, style int, date1904 bool) string {
	id := 0
	if style >= 0 && style < len(st.xf) {
		id = st.xf[style]
	}
	code, custom := st.codes[id]
	if xlsxIsDateFormat(id, code, custom) {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if s, ok := excelSerialText(f, date1904); ok {
				return s
			}
		}
		return v
	}
	if cur := xlsxCurrency(code); cur != "" {
		return v + " " + cur
	}
	return v
}

// xlsxIsDateFormat reports whether a number format renders a date or a time:
// the built-in date ids, or a code whose first section uses a date/time token
// outside quoted literals, escapes and bracketed modifiers.
func xlsxIsDateFormat(id int, code string, custom bool) bool {
	if !custom {
		return id >= 14 && id <= 22 || id >= 27 && id <= 36 || id >= 45 && id <= 47 || id >= 50 && id <= 58
	}
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case inQuote:
			if c == '"' {
				inQuote = false
			}
		case c == '"':
			inQuote = true
		case c == '\\', c == '_', c == '*':
			i++ // an escaped literal, a padding or a fill character
		case c == '[':
			j := strings.IndexByte(code[i:], ']')
			if j < 0 {
				i = len(code)
				continue
			}
			// [h], [mm], [ss] are elapsed time; [Red], [$-40C], [>100] are not.
			if inner := strings.ToLower(code[i+1 : i+j]); inner != "" && strings.Trim(inner, "hms") == "" {
				b.WriteByte('h')
			}
			i += j
		case c == ';':
			i = len(code) // the first section decides
		default:
			b.WriteByte(c)
		}
	}
	return strings.ContainsAny(strings.ToLower(b.String()), "dmyhs")
}

// xlsxCurrency is the ISO code of the currency a number format displays, from
// its first section: € and £ only — a bare $ names too many currencies.
func xlsxCurrency(code string) string {
	if i := strings.IndexByte(code, ';'); i >= 0 {
		code = code[:i]
	}
	upper := strings.ToUpper(code)
	switch {
	case strings.Contains(code, "€"), strings.Contains(upper, "EUR"):
		return "EUR"
	case strings.Contains(code, "£"), strings.Contains(upper, "GBP"):
		return "GBP"
	}
	return ""
}

// excelSerialText renders a date serial as ISO 8601: "2025-03-17", with
// "T06:12" (or "T06:12:30") when it carries a time, and "06:12" for a time
// alone. The 1900 system keeps Excel's fictitious 29 February 1900 out.
func excelSerialText(v float64, date1904 bool) (string, bool) {
	if math.IsNaN(v) || v < 0 || v >= 2958466 {
		return "", false
	}
	days := math.Floor(v)
	secs := int(math.Round((v - days) * 86400))
	if secs >= 86400 {
		days++
		secs -= 86400
	}
	clock := func(withSeconds bool) string {
		s := fmt.Sprintf("%02d:%02d", secs/3600, secs%3600/60)
		if withSeconds || secs%60 != 0 {
			s += fmt.Sprintf(":%02d", secs%60)
		}
		return s
	}
	if days == 0 && !date1904 {
		if secs == 0 {
			return "", false
		}
		return clock(false), true
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	switch {
	case date1904:
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	case days == 60:
		return "", false
	case days < 60:
		base = base.AddDate(0, 0, 1)
	}
	s := base.AddDate(0, 0, int(days)).Format("2006-01-02")
	if secs > 0 {
		s += "T" + clock(false)
	}
	return s, true
}

var reOOXMLEscape = regexp.MustCompile(`_x([0-9A-Fa-f]{4})_`)

// xlsxUnescape decodes the _xHHHH_ escapes OOXML uses for characters XML
// cannot carry (a carriage return is _x000D_, an underscore before such a
// sequence _x005F_).
func xlsxUnescape(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	return reOOXMLEscape.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:6], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

// parseCellRef reads "B4" / "$B$4" into (column 2, row 4).
func parseCellRef(ref string) (col, row int, ok bool) {
	ref = strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(ref)), "$", "")
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
	}
	if i == 0 || i > 3 {
		return 0, 0, false
	}
	if i == len(ref) {
		return col, 0, true
	}
	n, err := strconv.Atoi(ref[i:])
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	return col, n, true
}

// columnName is the spreadsheet column letters: 1 → A, 28 → AB.
func columnName(col int) string {
	var b []byte
	for col > 0 {
		col--
		b = append([]byte{byte('A' + col%26)}, b...)
		col /= 26
	}
	return string(b)
}

var errOOXMLPartMissing = errors.New("ooxml part missing")

// ooxmlPart reads one entry out of an OOXML package under a hard size cap.
// The cap is enforced on the DECOMPRESSED stream, so a lying header buys
// nothing.
func ooxmlPart(raw []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, runner.Permanent("ooxml_open_failed", "the document package could not be opened")
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, runner.Permanent("ooxml_open_failed", "a document part could not be opened")
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxIngestXMLBytes+1))
		if err != nil {
			return nil, runner.Permanent("ooxml_read_failed", "a document part could not be read")
		}
		if len(data) > maxIngestXMLBytes {
			return nil, runner.Permanent("ooxml_part_too_large", "a document part exceeds the allowed size")
		}
		return data, nil
	}
	return nil, errOOXMLPartMissing
}

// ---------------------------------------------------------------------------
// Shared step helpers
// ---------------------------------------------------------------------------

// loadIngestTargets reads the piece and its original through the tenant
// predicate. A step whose rows vanished is permanently failed, not retried.
func loadIngestTargets(repo *u.TenantRepo, in ingestStepInput) (*types.Piece, *types.Asset, error) {
	if in.PieceID == "" {
		return nil, nil, runner.Permanent("invalid_step_input", "the step names no piece")
	}
	var piece types.Piece
	if err := repo.Pieces().Where("id = ?", in.PieceID).First(&piece).Error; err != nil {
		return nil, nil, runner.Permanent("piece_not_found", "the piece no longer exists")
	}
	var asset types.Asset
	if err := repo.Assets().Where("id = ?", piece.SourceAssetID).First(&asset).Error; err != nil {
		return nil, nil, runner.Permanent("asset_not_found", "the stored original no longer exists")
	}
	return &piece, &asset, nil
}

// ingestFetch reads an object back out of the blob store.
func ingestFetch(ctx context.Context, key string) ([]byte, error) {
	rc, err := u.Blob.Get(ctx, key)
	if err != nil {
		if errors.Is(err, u.ErrBlobNotFound) {
			return nil, runner.Permanent("original_missing", "the stored original could not be found")
		}
		return nil, runner.Retry("blob_read_failed", "could not read the stored original")
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxIngestFileBytes+1))
	if err != nil {
		return nil, runner.Retry("blob_read_failed", "could not read the stored original")
	}
	if len(raw) > maxIngestFileBytes {
		return nil, runner.Permanent("original_too_large", "the stored original exceeds the allowed size")
	}
	return raw, nil
}

// ingestPutText stores a derived text object and records it as an asset, so
// the derived bytes are inventoried rather than floating unreferenced in the
// bucket. The key is derived from the piece, which makes a replayed attempt
// overwrite instead of duplicating (§4.3).
func ingestPutText(ctx context.Context, repo *u.TenantRepo, piece types.Piece, key, text string) error {
	return ingestPutObject(ctx, repo, piece, key, "text/plain; charset=utf-8", text)
}

// ingestPutRecord stores a piece's extraction record (-extract.json) next to
// its text: the versioned index locators are cut from.
func ingestPutRecord(ctx context.Context, repo *u.TenantRepo, piece types.Piece, rec extractionRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return runner.Permanent("record_encode_failed", "could not encode the extraction record")
	}
	return ingestPutObject(ctx, repo, piece, extractRecordKey(repo.TenantID(), piece.DossierID, piece.ID), "application/json", string(raw))
}

func ingestPutObject(ctx context.Context, repo *u.TenantRepo, piece types.Piece, key, mime, text string) error {
	if err := u.Blob.Put(ctx, key, strings.NewReader(text), int64(len(text)), mime); err != nil {
		ingestLog.Errorf("Derived text store failed (piece=%s chars=%d): %v", piece.ID, len(text), err)
		return runner.Retry("text_store_failed", "could not store the extracted text")
	}
	sum := sha256.Sum256([]byte(text))
	asset := types.Asset{
		ID:       uuid.NewString(),
		TenantID: repo.TenantID(),
		S3Key:    key,
		Kind:     types.AssetKindArtifact,
		Mime:     mime,
		Size:     int64(len(text)),
		SHA256:   hex.EncodeToString(sum[:]),
	}
	// The key is stable per piece, so a re-run updates the inventory row
	// rather than colliding on its unique index.
	if err := repo.Assets().Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "s3_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"size", "sha256", "mime"}),
	}).Create(&asset).Error; err != nil {
		ingestLog.Errorf("Derived text asset insert failed (piece=%s): %v", piece.ID, err)
		return runner.Retry("text_asset_failed", "could not record the extracted text")
	}
	return nil
}

// textSidecarKey is where a text-layer document's extracted text lives.
func textSidecarKey(tenantID, dossierID, pieceID string) string {
	return u.DossierKey(tenantID, dossierID, pieceID+"-text.txt")
}

// extractRecordKey is where a piece's extraction record lives: the versioned
// index of its sidecar text (paragraphs, cells) or, for a scan, its expected
// pages and OCR cap.
func extractRecordKey(tenantID, dossierID, pieceID string) string {
	return u.DossierKey(tenantID, dossierID, pieceID+"-extract.json")
}

// pageTextKey is where one OCR'd page's text lives.
func pageTextKey(tenantID, dossierID, pieceID string, page int) string {
	return u.DossierKey(tenantID, dossierID, fmt.Sprintf("%s-p%03d.txt", pieceID, page))
}

// pageImageKey is where the rasterised page a lawyer can inspect lives (§7.2:
// always keep the page image).
func pageImageKey(tenantID, dossierID, pieceID string, page int) string {
	return u.DossierKey(tenantID, dossierID, fmt.Sprintf("%s-p%03d.png", pieceID, page))
}

// ingestToolError maps a RunTool failure onto a step failure. A missing binary
// is a deployment problem, not a bad document: it must not burn the retries.
func ingestToolError(tool string, err error) error {
	switch {
	case u.IsToolMissing(err):
		return runner.Permanent(tool+"_missing", tool+" is not available in this image")
	case errors.Is(err, u.ErrToolTimeout):
		return runner.Retry(tool+"_timeout", tool+" exceeded its time limit")
	case errors.Is(err, u.ErrOutputTooLarge), errors.Is(err, u.ErrInputTooLarge):
		return runner.Permanent(tool+"_too_large", "the document exceeds the "+tool+" size limits")
	}
	// The tool's stderr is a diagnostic and must not be forwarded verbatim.
	ingestLog.Warnf("%s failed: %v", tool, err)
	return runner.Retry(tool+"_failed", tool+" could not process the document")
}

func ingestCharCount(pages []ingestPage) int {
	n := 0
	for _, p := range pages {
		n += len(p.Text)
	}
	return n
}

// ingestJoinPages rebuilds a form-feed-separated document from its pages, so
// the sidecar and the chunker agree on where pages start.
func ingestJoinPages(pages []ingestPage) string {
	parts := make([]string, 0, len(pages))
	for _, p := range pages {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\f")
}

// ingestWriteFile stages bytes for a CLI tool, owner-readable only.
func ingestWriteFile(p string, b []byte) error {
	return os.WriteFile(p, b, 0o600)
}
