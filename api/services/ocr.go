package services

// OCR — local by default, remote vision as a contractual mode (docs/PLAN.md §7.2).
//
// For confidential dossier pages the DEFAULT IS LOCAL: one ocr_page step per
// page, where `pdftoppm` rasterises the page and `tesseract` reads it with the
// French language pack, concurrency 1 (the ocr pool), with hard page / pixel /
// time limits. Both are CLIs, so the Go binary stays CGO_ENABLED=0.
//
// A vision LLM beats Tesseract on hard scans, but the gateway routes to
// several upstream providers, which is incompatible with any sovereignty claim
// for privileged client documents. So remote vision is an ADMINISTRATOR-ENABLED
// PROCESSING MODE (VISION_OCR_ENABLED). In that mode the scan is read in page
// RANGES (ocr_batch, llm pool): poppler cuts the range out of the original and
// the model receives it as a PDF, answering one <page n="k"> block per page.
//
// The fallback runs one way only: a page the model does not return — refused,
// cut off, missing — goes to local tesseract. Local never falls back to
// remote; when tesseract is missing or fails, the step FAILS and says so. It
// does not quietly ship the page to a third party.
//
// Two things are always recorded, because §7.2 is explicit that low-confidence
// OCR must never silently feed a filed pleading:
//
//   - the per-page confidence, in the step's output row (unknown for a vision
//     page, which is flagged for review like a low-confidence one), and
//   - for a tesseract page, the page IMAGE, kept as an asset so a lawyer can
//     inspect the source. A vision page's source is the original PDF itself.
//
// Nothing here logs OCR text. Confidence numbers, page indexes, byte counts
// and durations are shapes; the words on the page are not (§7.4).

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var ocrLog = u.NewLogger("services.ocr")

// Hard limits. OCR is the one step that can burn a pod's CPU budget, so every
// dimension is bounded: how many pages we will read at all, how large a
// rasterised page may get, and how long each child process may run.
const (
	maxOCRPages       = 200              // pages per document
	ocrRasterDPI      = "200"            // pdftoppm -r
	ocrMaxPixelSide   = "2200"           // pdftoppm -scale-to: bounds total pixels
	ocrRasterTimeout  = 90 * time.Second // per page
	ocrEngineTimeout  = 2 * time.Minute  // per page
	ocrMaxImageBytes  = 32 << 20         // one rasterised page
	ocrLowConfidence  = 70.0             // below this, the page is FLAGGED
	ocrVisionMaxChars = 40000            // cap on one vision page we will store

	// Vision ranges. The range size is the latency/robustness trade the
	// benchmark measures (ocr_bench_test.go): one call per range, ranges run
	// in parallel on the llm pool, and a range's answer must stay far below
	// the output limit — a dense page is ~1,500 tokens.
	ocrBatchPages       = 8
	ocrMaxTokensPerPage = 3000
	ocrMaxTokensPerCall = 32000
	ocrCutTimeout       = time.Minute
	ocrMaxRangeBytes    = 20 << 20 // one cut range, before base64
)

// ocrReasoningEffort caps the vision model's hidden reasoning. Transcription
// needs none, and on a scan it was most of the latency and the cost. A var so
// the benchmark can compare it against the provider default.
var ocrReasoningEffort = "low"

// ocrLanguage is the tesseract language pack. `fra` is the deployed default
// (docs/PLAN.md §7.2: tesseract-ocr-data-fra ships in the image); OCR_LANG
// exists so a machine without the French pack can still exercise the path.
var ocrLanguage = func() string {
	if v := strings.TrimSpace(os.Getenv("OCR_LANG")); v != "" {
		return v
	}
	return "fra"
}()

// ocrCfg reads the deployment configuration once. VisionOCREnabled and
// VisionModel are the only fields this file cares about, and both are
// administrator decisions rather than per-request options.
var ocrCfg = sync.OnceValue(config.Load)

// ocrPageOutput is the step's jsonb output: the durable record that this page
// was read, how well, by what, and where its image lives.
type ocrPageOutput struct {
	Page            int     `json:"page"`
	Chars           int     `json:"chars"`
	Engine          string  `json:"engine"`
	Confidence      float64 `json:"confidence"`
	ConfidenceKnown bool    `json:"confidence_known"`
	LowConfidence   bool    `json:"low_confidence"`
	ImageAssetID    string  `json:"image_asset_id"`
	ImageBytes      int     `json:"image_bytes"`
}

// stepOCRPage reads ONE page of a scanned PDF with local tesseract — the
// default mode, and the fallback for any page the vision model did not return.
// One page per step is what keeps the unit small, idempotent and leasable: a
// pod that dies during page 30 of 40 costs one page, not the document.
func stepOCRPage(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in ingestStepInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	if in.Page <= 0 || in.Page > maxOCRPages {
		return nil, runner.Permanent("invalid_page", "the step names no readable page")
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
	image, err := rasterisePage(ctx, raw, in.Page)
	if err != nil {
		return nil, err
	}

	// The page image is kept whatever the OCR result: it is the source a
	// lawyer checks a doubtful line against (§7.2).
	imageAssetID, err := storePageImage(ctx, repo, *piece, in.Page, image)
	if err != nil {
		return nil, err
	}

	text, confidence, err := tesseractOCR(ctx, image)
	if err != nil {
		return nil, err
	}
	known, engine := true, "tesseract"

	if err := ingestPutText(ctx, repo, *piece,
		pageTextKey(repo.TenantID(), piece.DossierID, piece.ID, in.Page), text); err != nil {
		return nil, err
	}

	out := ocrPageOutput{
		Page:            in.Page,
		Chars:           len(text),
		Engine:          engine,
		Confidence:      confidence,
		ConfidenceKnown: known,
		LowConfidence:   !known || confidence < ocrLowConfidence,
		ImageAssetID:    imageAssetID,
		ImageBytes:      len(image),
	}

	// FLAGGED, never silently accepted: the run event is what the plan
	// monitor renders next to the page, and the output row is what an export
	// review reads. Neither carries a word of the page.
	if out.LowConfidence {
		ocrLog.Warnf("Low-confidence OCR (piece=%s page=%d engine=%s confidence=%.1f known=%t chars=%d)",
			piece.ID, in.Page, engine, confidence, known, len(text))
		if err := sc.Emit(types.EventStepProgress, map[string]any{
			"piece_id": piece.ID, "page": in.Page, "engine": engine,
			"confidence": confidence, "confidence_known": known, "low_confidence": true,
		}); err != nil {
			ocrLog.Warnf("Could not emit the low-confidence event (piece=%s page=%d): %v", piece.ID, in.Page, err)
		}
	}

	ocrLog.Infof("OCR page done (piece=%s page=%d engine=%s confidence=%.1f chars=%d image_bytes=%d %s)",
		piece.ID, in.Page, engine, confidence, len(text), len(image), time.Since(started).Round(time.Millisecond))
	return out, nil
}

// rasterisePage renders one page to PNG with pdftoppm. -scale-to bounds the
// pixel count (a 200 dpi A0 plan would otherwise be a 400-megapixel bitmap),
// -singlefile makes the output name predictable, and the whole thing runs
// through RunTool: argument array, per-job temp dir, hard timeout.
func rasterisePage(ctx context.Context, pdf []byte, page int) ([]byte, error) {
	dir, cleanup, err := u.JobDir("ocr")
	if err != nil {
		return nil, runner.Retry("jobdir_failed", "could not create a work directory")
	}
	defer cleanup()

	src := path.Join(dir, "in.pdf")
	if err := ingestWriteFile(src, pdf); err != nil {
		return nil, runner.Retry("jobdir_failed", "could not stage the document")
	}
	out := path.Join(dir, "page")

	p := strconv.Itoa(page)
	if _, err := u.RunTool(ctx, u.ToolSpec{
		Name: "pdftoppm",
		Args: []string{
			"-png", "-singlefile",
			"-f", p, "-l", p,
			"-r", ocrRasterDPI, "-scale-to", ocrMaxPixelSide,
			src, out,
		},
		Dir:        dir,
		InputPaths: []string{src},
		Timeout:    ocrRasterTimeout,
	}); err != nil {
		return nil, ingestToolError("pdftoppm", err)
	}

	image, err := os.ReadFile(out + ".png")
	if err != nil {
		return nil, runner.Permanent("page_not_rendered", "the page could not be rasterised")
	}
	if len(image) > ocrMaxImageBytes {
		return nil, runner.Permanent("page_image_too_large", "the rasterised page exceeds the allowed size")
	}
	return image, nil
}

// storePageImage keeps the rasterised page as a page_image asset. The key is
// derived from the piece and page number, so a replayed attempt overwrites the
// same object instead of littering the bucket (§4.3).
func storePageImage(ctx context.Context, repo *u.TenantRepo, piece types.Piece, page int, image []byte) (string, error) {
	key := pageImageKey(repo.TenantID(), piece.DossierID, piece.ID, page)
	if err := u.Blob.Put(ctx, key, bytes.NewReader(image), int64(len(image)), "image/png"); err != nil {
		ocrLog.Errorf("Page image store failed (piece=%s page=%d bytes=%d): %v", piece.ID, page, len(image), err)
		return "", runner.Retry("page_image_store_failed", "could not store the page image")
	}
	asset := types.Asset{
		ID:       uuid.NewString(),
		TenantID: repo.TenantID(),
		S3Key:    key,
		Kind:     types.AssetKindPageImage,
		Mime:     "image/png",
		Size:     int64(len(image)),
	}
	if err := repo.Assets().Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "s3_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"size", "mime"}),
	}).Create(&asset).Error; err != nil {
		ocrLog.Errorf("Page image asset insert failed (piece=%s page=%d): %v", piece.ID, page, err)
		return "", runner.Retry("page_image_asset_failed", "could not record the page image")
	}
	// Re-read so a conflicting insert returns the id that actually exists.
	var stored types.Asset
	if err := repo.Assets().Where("s3_key = ?", key).First(&stored).Error; err == nil {
		return stored.ID, nil
	}
	return asset.ID, nil
}

// tesseractOCR runs tesseract over one page image and derives both the text
// and the mean word confidence from a single TSV pass.
func tesseractOCR(ctx context.Context, image []byte) (string, float64, error) {
	dir, cleanup, err := u.JobDir("tesseract")
	if err != nil {
		return "", 0, runner.Retry("jobdir_failed", "could not create a work directory")
	}
	defer cleanup()

	src := path.Join(dir, "page.png")
	if err := ingestWriteFile(src, image); err != nil {
		return "", 0, runner.Retry("jobdir_failed", "could not stage the page image")
	}

	// `tsv` gives words with per-word confidence, so text and confidence come
	// from ONE invocation rather than two passes that could disagree.
	stdout, err := u.RunTool(ctx, u.ToolSpec{
		Name:       "tesseract",
		Args:       []string{src, "-", "-l", ocrLanguage, "--psm", "1", "tsv"},
		Dir:        dir,
		InputPaths: []string{src},
		Timeout:    ocrEngineTimeout,
	})
	if err != nil {
		return "", 0, ingestToolError("tesseract", err)
	}
	text, confidence := parseTesseractTSV(string(stdout))
	return text, confidence, nil
}

// parseTesseractTSV turns tesseract's TSV into text plus the mean confidence
// of the words it actually recognised. Layout columns (level < 5) carry a
// confidence of -1 and no text; they are structure, not reading.
func parseTesseractTSV(tsv string) (string, float64) {
	var b strings.Builder
	var sum float64
	var n int
	lastLine := -1

	for i, row := range strings.Split(tsv, "\n") {
		if i == 0 || strings.TrimSpace(row) == "" {
			continue // header
		}
		cols := strings.Split(row, "\t")
		if len(cols) < 12 {
			continue
		}
		word := strings.TrimRight(cols[11], "\r")
		if strings.TrimSpace(word) == "" {
			continue
		}
		conf, err := strconv.ParseFloat(strings.TrimSpace(cols[10]), 64)
		if err != nil || conf < 0 {
			continue
		}
		lineNo, _ := strconv.Atoi(strings.TrimSpace(cols[4]))
		switch {
		case lastLine == -1:
		case lineNo != lastLine:
			b.WriteString("\n")
		default:
			b.WriteString(" ")
		}
		lastLine = lineNo
		b.WriteString(word)
		sum += conf
		n++
	}
	if n == 0 {
		return b.String(), 0
	}
	return b.String(), sum / float64(n)
}

// ---------------------------------------------------------------------------
// Remote vision: page ranges
// ---------------------------------------------------------------------------

// ocrFanOut plans the reading of a scan: page ranges for the vision model when
// an administrator enabled it, one tesseract step per page otherwise.
func ocrFanOut(pieceID, assetID string, pages int) []runner.StepSpec {
	var specs []runner.StepSpec
	if !ocrCfg().VisionOCREnabled {
		for p := 1; p <= pages; p++ {
			specs = append(specs, runner.StepSpec{
				Key:   fmt.Sprintf("%s:%s:%d", StepKindOCRPage, pieceID, p),
				Kind:  StepKindOCRPage,
				Label: fmt.Sprintf("OCR page %d", p),
				Input: ingestStepInput{PieceID: pieceID, AssetID: assetID, Page: p, PageCount: pages},
			})
		}
		return specs
	}
	for from := 1; from <= pages; from += ocrBatchPages {
		to := min(from+ocrBatchPages-1, pages)
		specs = append(specs, runner.StepSpec{
			Key:   fmt.Sprintf("%s:%s:%d-%d", StepKindOCRBatch, pieceID, from, to),
			Kind:  StepKindOCRBatch,
			Label: fmt.Sprintf("OCR pages %d à %d", from, to),
			Input: ingestStepInput{PieceID: pieceID, AssetID: assetID, Page: from, PageTo: to, PageCount: pages},
		})
	}
	return specs
}

// ocrBatchOutput is an ocr_batch step's jsonb output: the pages the model
// read, and the ones it handed to local OCR and why. Shapes only.
type ocrBatchOutput struct {
	From           int             `json:"from"`
	To             int             `json:"to"`
	Model          string          `json:"model"`
	Pages          []ocrPageOutput `json:"pages"`
	Fallback       []int           `json:"fallback,omitempty"`
	FallbackReason string          `json:"fallback_reason,omitempty"`
}

// stepOCRBatch reads a RANGE of a scanned PDF with the vision model. The range
// is cut out of the original and sent as a PDF — the scan at its own
// resolution, no rasterising here — and a range rather than the whole document
// keeps one answer far below the output limit while several ranges run at once.
//
// Nothing the model does fails the run. A page it does not return — a refusal,
// a truncated answer, a missing block — is read by local OCR instead: this
// step enqueues an ocr_page step for it and makes the chunking step wait. A
// blank page is not a failure either; it is an empty block. Only an outage on
// our side (blob store, database) is an error, and an unreachable gateway is
// retried before its pages fall back.
func stepOCRBatch(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in ingestStepInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	if in.Page <= 0 || in.PageTo < in.Page || in.PageTo > maxOCRPages {
		return nil, runner.Permanent("invalid_page", "the step names no readable page range")
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
	n := in.PageTo - in.Page + 1
	model := ocrCfg().VisionModel
	out := ocrBatchOutput{From: in.Page, To: in.PageTo, Model: model, Pages: []ocrPageOutput{}}

	var read map[int]string
	var part []byte
	enabled := ocrCfg().VisionOCREnabled
	if enabled {
		part, err = cutPDF(ctx, raw, in.Page, in.PageTo)
	}
	switch {
	case !enabled:
		// Switched off since this range was planned: the administrator's
		// decision wins over the plan, and the pages are read locally.
		out.FallbackReason = "vision_disabled"
	case err != nil:
		out.FallbackReason = "cut_failed"
		ocrLog.Warnf("Could not cut a range out of the scan (piece=%s range=%d-%d): %v", piece.ID, in.Page, in.PageTo, err)
	case len(part) > ocrMaxRangeBytes:
		out.FallbackReason = "range_too_large"
	default:
		read, _, err = visionReadPDF(ctx, u.ChatOpts{
			Model:         model,
			TenantID:      sc.TenantID,
			RunID:         sc.Run.ID,
			RunStepID:     sc.Step.ID,
			StepAttemptID: sc.Attempt.ID,
		}, part, n)
		if err != nil {
			// A gateway that is down is worth the step's own retries; once
			// they are spent, the pages fall back like any other failure.
			if u.IsRetryable(err) && sc.Attempt.Number < sc.Step.MaxAttempts {
				return nil, runner.Retry("vision_ocr_unavailable", "the vision model could not be reached")
			}
			out.FallbackReason = visionFailure(err)
		}
	}

	for i := 1; i <= n; i++ {
		page := in.Page + i - 1
		text, ok := read[i]
		if !ok {
			out.Fallback = append(out.Fallback, page)
			continue
		}
		if err := ingestPutText(ctx, repo, *piece,
			pageTextKey(repo.TenantID(), piece.DossierID, piece.ID, page), text); err != nil {
			return nil, err
		}
		// A vision model reports no confidence, so the page is flagged for
		// review exactly like a low-confidence tesseract page.
		out.Pages = append(out.Pages, ocrPageOutput{
			Page: page, Chars: len(text), Engine: "vision", LowConfidence: true,
		})
	}

	if len(out.Fallback) > 0 {
		if out.FallbackReason == "" {
			out.FallbackReason = "pages_missing"
		}
		if err := enqueueLocalOCR(ctx, repo, sc.Run.ID, *piece, in.AssetID, in.PageCount, out.Fallback); err != nil {
			return nil, err
		}
		ocrLog.Warnf("Vision OCR handed pages to local OCR (piece=%s range=%d-%d fallback=%d reason=%s)",
			piece.ID, in.Page, in.PageTo, len(out.Fallback), out.FallbackReason)
	}

	ocrLog.Infof("OCR range done (piece=%s range=%d-%d model=%s read=%d fallback=%d %s)",
		piece.ID, in.Page, in.PageTo, model, len(out.Pages), len(out.Fallback), time.Since(started).Round(time.Millisecond))
	return out, nil
}

// enqueueLocalOCR hands pages to tesseract: one ocr_page step per page, and an
// edge from the chunking step onto each. The edges are drawn before the
// calling step returns, so the chunker — which waits on that step — can never
// run in the window between the two writes. A replayed attempt converges:
// existing steps are pruned and DependOn ignores an edge that already exists.
func enqueueLocalOCR(ctx context.Context, repo *u.TenantRepo, runID string, piece types.Piece,
	assetID string, pageCount int, pages []int) error {
	specs := make([]runner.StepSpec, 0, len(pages))
	keys := make([]string, 0, len(pages))
	for _, p := range pages {
		key := fmt.Sprintf("%s:%s:%d", StepKindOCRPage, piece.ID, p)
		keys = append(keys, key)
		specs = append(specs, runner.StepSpec{
			Key:   key,
			Kind:  StepKindOCRPage,
			Label: fmt.Sprintf("OCR locale page %d", p),
			Input: ingestStepInput{PieceID: piece.ID, AssetID: assetID, Page: p, PageCount: pageCount},
		})
	}
	if pruned := pruneExistingSteps(repo, runID, specs); len(pruned) > 0 {
		if _, err := runner.EnqueueSteps(ctx, runID, pruned); err != nil {
			return runner.Retry("enqueue_failed", "could not hand the pages to local OCR")
		}
	}
	if err := runner.DependOn(ctx, runID, StepKindChunkAndEmbed+":"+piece.ID, keys); err != nil {
		return runner.Retry("depend_failed", "could not make the chunking step wait for local OCR")
	}
	return nil
}

// cutPDF extracts pages from..to (1-based, inclusive) of a PDF as a new PDF,
// with poppler's pdfseparate + pdfunite: the pages are copied, not re-rendered.
func cutPDF(ctx context.Context, pdf []byte, from, to int) ([]byte, error) {
	dir, cleanup, err := u.JobDir("ocr-cut")
	if err != nil {
		return nil, runner.Retry("jobdir_failed", "could not create a work directory")
	}
	defer cleanup()

	src := path.Join(dir, "in.pdf")
	if err := ingestWriteFile(src, pdf); err != nil {
		return nil, runner.Retry("jobdir_failed", "could not stage the document")
	}
	if _, err := u.RunTool(ctx, u.ToolSpec{
		Name:       "pdfseparate",
		Args:       []string{"-f", strconv.Itoa(from), "-l", strconv.Itoa(to), src, path.Join(dir, "p-%d.pdf")},
		Dir:        dir,
		InputPaths: []string{src},
		Timeout:    ocrCutTimeout,
	}); err != nil {
		return nil, ingestToolError("pdfseparate", err)
	}

	parts := make([]string, 0, to-from+1)
	for p := from; p <= to; p++ {
		parts = append(parts, path.Join(dir, fmt.Sprintf("p-%d.pdf", p)))
	}
	out := parts[0]
	if len(parts) > 1 {
		out = path.Join(dir, "range.pdf")
		if _, err := u.RunTool(ctx, u.ToolSpec{
			Name:       "pdfunite",
			Args:       append(parts, out),
			Dir:        dir,
			InputPaths: parts,
			Timeout:    ocrCutTimeout,
		}); err != nil {
			return nil, ingestToolError("pdfunite", err)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, runner.Permanent("range_not_cut", "the page range could not be extracted")
	}
	return b, nil
}

// ocrVisionPrompt is STATIC: the page count and the document
// travel in the user message.
const ocrVisionPrompt = `<role>
Tu es un moteur de transcription de documents juridiques français numérisés. Tu transcris, tu
n'interprètes pas.
</role>
<tasks>
1. Le message joint un PDF et en annonce le nombre de pages. Transcris chaque page, dans l'ordre,
   sans en omettre aucune.
2. Pour chaque page, émets exactement un bloc <page n="k">…</page>, où k est le numéro de la page
   dans le PDF joint, de 1 au nombre annoncé. Une page blanche ou sans texte donne un bloc vide.
3. Conserve l'ordre de lecture, les sauts de ligne et la ponctuation d'origine. N'ajoute aucun
   commentaire, aucun titre, aucune correction, aucune reformulation, aucun Markdown.
4. Transcris un tableau ligne par ligne, cellules séparées par « | ».
5. Marque un passage illisible par [illisible] plutôt que de le deviner, et une signature
   manuscrite par [signature].
</tasks>
<output>
Uniquement les blocs <page n="k">…</page>, de la page 1 à la dernière, rien avant, entre ni après.
</output>`

var ocrPageBlock = regexp.MustCompile(`(?s)<page n="(\d+)">(.*?)</page>`)

// visionReadPDF sends a PDF of `pages` pages to the vision model and returns
// the text of each page it answered, keyed 1..pages. A truncated answer still
// returns the pages it completed, alongside ErrTruncated.
func visionReadPDF(ctx context.Context, opts u.ChatOpts, pdf []byte, pages int) (map[int]string, u.ChatResult, error) {
	if !u.LLMConfigured() {
		return nil, u.ChatResult{}, u.ErrLLMNotConfigured
	}
	opts.System = ocrVisionPrompt
	opts.User = fmt.Sprintf(`<document pages="%d">Transcris les %d pages de ce PDF.</document>`, pages, pages)
	opts.Parts = []u.ContentPart{{
		Kind:     "file",
		Filename: "pages.pdf",
		FileData: "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf),
	}}
	opts.Purpose = types.LLMPurposeVisionOCR
	opts.ReasoningEffort = ocrReasoningEffort
	opts.MaxTokens = min(pages*ocrMaxTokensPerPage, ocrMaxTokensPerCall)

	res, err := u.Chat(ctx, opts)
	if err != nil && !errors.Is(err, u.ErrTruncated) {
		return nil, res, err
	}
	read := parsePageBlocks(res.Text, pages)
	ocrLog.Infof("Vision OCR range read (model=%s pages=%d answered=%d prompt_tokens=%d completion_tokens=%d)",
		res.Model, pages, len(read), res.PromptTokens, res.CompletionTokens)
	return read, res, err
}

// parsePageBlocks maps the answer's <page n="k"> blocks to pages 1..n. A block
// outside the range or repeated is ignored, and an unclosed one (a cut-off
// answer) never matches: a page with no block is absent from the map, which is
// what sends it to local OCR. A one-page answer the model left untagged is
// taken whole.
func parsePageBlocks(text string, n int) map[int]string {
	out := make(map[int]string, n)
	for _, m := range ocrPageBlock.FindAllStringSubmatch(text, -1) {
		k, err := strconv.Atoi(m[1])
		if err != nil || k < 1 || k > n {
			continue
		}
		if _, dup := out[k]; !dup {
			out[k] = capPageText(strings.Trim(m[2], "\r\n"))
		}
	}
	if n == 1 && len(out) == 0 && !strings.Contains(text, "<page") && strings.TrimSpace(text) != "" {
		out[1] = capPageText(strings.TrimSpace(text))
	}
	return out
}

func capPageText(s string) string {
	if len(s) <= ocrVisionMaxChars {
		return s
	}
	return strings.ToValidUTF8(s[:ocrVisionMaxChars], "")
}

// visionFailure names why a range fell back, for the step output and the log.
func visionFailure(err error) string {
	var ge *u.GatewayError
	switch {
	case errors.Is(err, u.ErrRefused):
		return "refused"
	case errors.Is(err, u.ErrTruncated):
		return "truncated"
	case errors.Is(err, u.ErrEmptyCompletion):
		return "empty"
	case errors.Is(err, u.ErrLLMNotConfigured):
		return "gateway_not_configured"
	case errors.As(err, &ge):
		return fmt.Sprintf("gateway_%d", ge.Status)
	}
	return "vision_failed"
}

// ocrPageSummary aggregates the per-page OCR outputs of one piece so the
// chunking step can flag a document whose reading is doubtful.
type ocrPageSummary struct {
	Pages          int     `json:"pages"`
	LowConfidence  int     `json:"low_confidence_pages"`
	MeanConfidence float64 `json:"mean_confidence"`
	MinConfidence  float64 `json:"min_confidence"`
}

// summariseOCR reads the durable step outputs rather than passing confidence
// through the step inputs: the rows are the record, and a re-run of the
// chunking step must see the same numbers. A page is in exactly one output —
// its vision range, or the ocr_page step it fell back to.
func summariseOCR(repo *u.TenantRepo, runID, pieceID string) ocrPageSummary {
	var steps []types.RunStep
	if err := repo.RunSteps().
		Where("run_id = ? AND ((kind = ? AND step_key LIKE ?) OR (kind = ? AND step_key LIKE ?))", runID,
			StepKindOCRPage, StepKindOCRPage+":"+pieceID+":%",
			StepKindOCRBatch, StepKindOCRBatch+":"+pieceID+":%").
		Find(&steps).Error; err != nil {
		ocrLog.Warnf("OCR summary query failed (piece=%s): %v", pieceID, err)
		return ocrPageSummary{}
	}
	var pages []ocrPageOutput
	for _, s := range steps {
		if len(s.Output) == 0 {
			continue
		}
		if s.Kind == StepKindOCRBatch {
			var out ocrBatchOutput
			if err := jsonUnmarshalStepOutput(s.Output, &out); err == nil {
				pages = append(pages, out.Pages...)
			}
			continue
		}
		var out ocrPageOutput
		if err := jsonUnmarshalStepOutput(s.Output, &out); err == nil {
			pages = append(pages, out)
		}
	}

	sum := ocrPageSummary{MinConfidence: 100}
	var total float64
	for _, out := range pages {
		sum.Pages++
		total += out.Confidence
		if out.Confidence < sum.MinConfidence {
			sum.MinConfidence = out.Confidence
		}
		if out.LowConfidence {
			sum.LowConfidence++
		}
	}
	if sum.Pages > 0 {
		sum.MeanConfidence = total / float64(sum.Pages)
	} else {
		sum.MinConfidence = 0
	}
	return sum
}

// ocrPageTexts reads back the page texts an OCR fan-out produced, in page
// order. A page whose object is missing is skipped rather than failing the
// document: a partially readable scan is still worth chunking, and the gap is
// visible in the per-page outputs.
func ocrPageTexts(ctx context.Context, repo *u.TenantRepo, piece types.Piece, pages int) []ingestPage {
	out := make([]ingestPage, 0, pages)
	for p := 1; p <= pages; p++ {
		raw, err := ingestFetch(ctx, pageTextKey(repo.TenantID(), piece.DossierID, piece.ID, p))
		if err != nil {
			ocrLog.Warnf("OCR page text missing (piece=%s page=%d)", piece.ID, p)
			continue
		}
		out = append(out, ingestPage{Number: p, Text: string(raw)})
	}
	return out
}

// fmtConfidence keeps the one place a confidence reaches a log line honest
// about unknown values.
func fmtConfidence(c float64, known bool) string {
	if !known {
		return "unknown"
	}
	return fmt.Sprintf("%.1f", c)
}
