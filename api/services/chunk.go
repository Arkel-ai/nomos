package services

// Chunking and embedding (docs/PLAN.md §7.1 step 5, §6.1).
//
// The extracted text becomes `piece_chunks`: ~800-token windows with 100
// tokens of overlap, each one carrying the PAGE RANGE it came from — because a
// private-corpus hit is only useful if the lawyer can be told which page of
// which piece to open, and a citation without a locator is not a citation
// (§6.4).
//
// Two details that are easy to get wrong and expensive to fix:
//
//   - `search_vector` is a plain tsvector column, NOT a generated one, so the
//     INSERT has to populate it with to_tsvector('french', text). GORM's
//     Create cannot express that, which is why this file writes one explicit
//     statement instead.
//   - the embedding is halfvec(1536) pinned to openai/text-embedding-3-small.
//     utils.Embed rejects any
//     model that answers with a different width, so a mismatched model fails
//     here rather than silently poisoning the corpus (§9.1(2)).
//
// Nothing in this file logs chunk text: counts, token estimates, page numbers
// and model names only (§7.4).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var chunkLog = u.NewLogger("services.chunk")

const (
	// chunkTargetTokens / chunkOverlapTokens are §7.1's ~800 / 100.
	chunkTargetTokens  = 800
	chunkOverlapTokens = 100
	// chunkCharsPerToken is the estimator. There is no tokenizer for bge-m3
	// in this build and adding one would be a dependency for a number that
	// only has to be approximately right: French prose runs ~4 characters per
	// token. The bound that matters (the model's context) is enforced by the
	// gateway, not by this estimate.
	chunkCharsPerToken = 4
	// chunkEmbedBatch bounds one embeddings request. utils.Embed batches
	// internally too; this keeps a very long document from building one
	// enormous slice in memory first.
	chunkEmbedBatch = 64
	// maxChunksPerPiece stops a pathological document (a 500-page scan of a
	// register) from turning into an unbounded write.
	maxChunksPerPiece = 2000
)

// chunkOutput is the step's jsonb output: shapes only.
type chunkOutput struct {
	Chunks         int             `json:"chunks"`
	Tokens         int             `json:"tokens"`
	Pages          int             `json:"pages"`
	EmbeddingModel string          `json:"embedding_model"`
	Dims           int             `json:"dims"`
	Truncated      bool            `json:"truncated,omitempty"`
	OCR            *ocrPageSummary `json:"ocr,omitempty"`
}

// stepChunkAndEmbed turns one piece's extracted text into piece_chunks rows.
//
// It is idempotent by construction: chunks are keyed (piece_id, ordinal) and
// upserted, and any tail left over from a longer previous run is deleted. A
// replayed attempt converges on the same rows rather than duplicating them
// (§4.3).
func stepChunkAndEmbed(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in ingestStepInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	piece, _, err := loadIngestTargets(repo, in)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	var pages []ingestPage
	out := chunkOutput{}

	if in.Scanned {
		// A scanned document's text is what the OCR fan-out produced, page by
		// page. Its confidence travels with it: a doubtful reading is flagged
		// on the chunking step too, so nothing downstream can mistake it for
		// clean text.
		pages = ocrPageTexts(ctx, repo, *piece, in.PageCount)
		summary := summariseOCR(repo, sc.Run.ID, piece.ID)
		out.OCR = &summary
		if summary.LowConfidence > 0 {
			chunkLog.Warnf("Chunking OCR text with low-confidence pages (piece=%s pages=%d low=%d mean=%s)",
				piece.ID, summary.Pages, summary.LowConfidence, fmtConfidence(summary.MeanConfidence, summary.Pages > 0))
			if err := sc.Emit(types.EventStepProgress, map[string]any{
				"piece_id": piece.ID, "low_confidence_pages": summary.LowConfidence,
				"pages": summary.Pages, "mean_confidence": summary.MeanConfidence,
			}); err != nil {
				chunkLog.Warnf("Could not emit the OCR summary event (piece=%s): %v", piece.ID, err)
			}
		}
	} else {
		raw, err := ingestFetch(ctx, textSidecarKey(repo.TenantID(), piece.DossierID, piece.ID))
		if err != nil {
			return nil, err
		}
		pages = splitFormFeeds(string(raw))
	}

	if in.Scanned {
		// The extract step recorded zero characters for a scan, because there
		// was no text layer to count. Now that the pages have been read, the
		// piece's shapes must say what the document actually holds.
		chars := ingestCharCount(pages)
		if err := repo.Pieces().Where("id = ?", piece.ID).Updates(map[string]any{
			"text_chars": chars, "page_count": len(pages),
		}).Error; err != nil {
			chunkLog.Warnf("Could not record the OCR text shape (piece=%s): %v", piece.ID, err)
		}
	}

	chunks := chunkPages(pages, chunkTargetTokens, chunkOverlapTokens)
	if len(chunks) > maxChunksPerPiece {
		chunks = chunks[:maxChunksPerPiece]
		out.Truncated = true
		chunkLog.Warnf("Chunk list truncated at %d (piece=%s)", maxChunksPerPiece, piece.ID)
	}
	out.Pages = len(pages)
	out.Chunks = len(chunks)
	for _, ch := range chunks {
		out.Tokens += ch.Tokens
	}
	if len(chunks) == 0 {
		chunkLog.Warnf("No chunkable text (piece=%s pages=%d scanned=%t)", piece.ID, len(pages), in.Scanned)
		return out, nil
	}

	if !u.LLMConfigured() {
		return nil, runner.Permanent("embeddings_not_configured", "the embeddings gateway is not configured")
	}
	model := ocrCfg().EmbeddingModel
	out.EmbeddingModel = model

	// Every gateway call made inside a step must be attributable to the
	// attempt that made it (§4.6). Embed's signature is deliberately narrow,
	// so the coordinates ride on the context.
	callCtx := u.WithCallRef(ctx, u.CallRef{
		TenantID:      sc.TenantID,
		RunID:         sc.Run.ID,
		RunStepID:     sc.Step.ID,
		StepAttemptID: sc.Attempt.ID,
	})

	for start := 0; start < len(chunks); start += chunkEmbedBatch {
		end := min(start+chunkEmbedBatch, len(chunks))
		texts := make([]string, 0, end-start)
		for _, ch := range chunks[start:end] {
			texts = append(texts, ch.Text)
		}
		vectors, err := u.Embed(callCtx, model, texts)
		if err != nil {
			if u.IsRetryable(err) {
				return nil, runner.Retry("embed_failed", "the embeddings gateway could not be reached")
			}
			return nil, runner.Permanent("embed_failed", "the embeddings call failed")
		}
		for i := range vectors {
			ch := chunks[start+i]
			if err := writePieceChunk(ctx, sc.DB(), *piece, start+i, ch, vectors[i], model); err != nil {
				return nil, err
			}
			if out.Dims == 0 {
				out.Dims = len(vectors[i])
			}
		}
		if err := sc.Heartbeat(); err != nil {
			return nil, runner.Retry("lease_lost", "the step lease expired while embedding")
		}
	}

	// A shorter re-run must not leave the tail of a longer previous one
	// behind: those rows would answer searches with text this piece no longer
	// contains.
	if err := repo.PieceChunks().
		Where("piece_id = ? AND ordinal >= ?", piece.ID, len(chunks)).
		Delete(&types.PieceChunk{}).Error; err != nil {
		chunkLog.Errorf("Stale chunk cleanup failed (piece=%s): %v", piece.ID, err)
		return nil, runner.Retry("chunk_cleanup_failed", "could not remove superseded chunks")
	}

	chunkLog.Infof("Chunked and embedded (piece=%s dossier=%s pages=%d chunks=%d tokens=%d model=%s dims=%d %s)",
		piece.ID, piece.DossierID, out.Pages, out.Chunks, out.Tokens, model, out.Dims,
		time.Since(started).Round(time.Millisecond))
	return out, nil
}

// writePieceChunk upserts one chunk row.
//
// This is the one place in the ingestion path that writes SQL by hand, and the
// reason is narrow: `search_vector` must be populated with
// to_tsvector('french', text) in the same statement, which no GORM tag or
// Create call can express. tenant_id is supplied from the run's tenant — the
// same value the repository would stamp — and never from anything a caller
// sent (§9.1(12)).
func writePieceChunk(ctx context.Context, db *gorm.DB, piece types.Piece, ordinal int,
	ch chunkSpec, vector []float32, model string) error {

	sum := sha256.Sum256([]byte(ch.Text))
	embedding := pgvector.NewHalfVector(vector)

	err := db.WithContext(ctx).Exec(`
		INSERT INTO piece_chunks
		    (tenant_id, dossier_id, piece_id, ordinal, page_from, page_to,
		     text, text_sha256, search_vector, embedding, embedding_model,
		     token_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, to_tsvector('french', ?), ?, ?, ?,
		        EXTRACT(EPOCH FROM now())::bigint)
		ON CONFLICT (piece_id, ordinal) DO UPDATE SET
		    page_from       = EXCLUDED.page_from,
		    page_to         = EXCLUDED.page_to,
		    text            = EXCLUDED.text,
		    text_sha256     = EXCLUDED.text_sha256,
		    search_vector   = EXCLUDED.search_vector,
		    embedding       = EXCLUDED.embedding,
		    embedding_model = EXCLUDED.embedding_model,
		    token_count     = EXCLUDED.token_count`,
		piece.TenantID, piece.DossierID, piece.ID, ordinal, ch.PageFrom, ch.PageTo,
		ch.Text, hex.EncodeToString(sum[:]), ch.Text, embedding, model, ch.Tokens).Error
	if err != nil {
		chunkLog.Errorf("Chunk insert failed (piece=%s ordinal=%d): %v", piece.ID, ordinal, err)
		return runner.Retry("chunk_insert_failed", "could not write the chunk")
	}
	return nil
}

// ---------------------------------------------------------------------------
// The chunker
// ---------------------------------------------------------------------------

// chunkSpec is one window of text plus the pages it spans.
type chunkSpec struct {
	Text     string
	PageFrom int
	PageTo   int
	Tokens   int
	// StartPage/StartOffset is where the chunk's first paragraph begins in
	// the extracted text: its page and code-point offset in that page. A
	// chunk opening on overlap carried from the previous one points where
	// that text actually is (piece_locators.go maps it onto a segment).
	StartPage, StartOffset int
}

// chunkSegment is the smallest unit the chunker moves around: one paragraph,
// tagged with the page it came from and its exact source range there.
// Chunk boundaries fall between segments, which is what keeps a chunk from
// starting mid-sentence; carrying a segment into the next chunk as overlap
// carries its source range with it, so overlap never shifts an offset.
type chunkSegment struct {
	text   string
	page   int
	tokens int
	// start/end are half-open code-point offsets in the page text, from the
	// paragraph's first to its last non-space character.
	start, end int
}

// chunkPages splits a document into ~target-token windows with `overlap`
// tokens of context carried into the next one, preserving page ranges.
func chunkPages(pages []ingestPage, target, overlap int) []chunkSpec {
	segments := segmentPages(pages, target)
	if len(segments) == 0 {
		return nil
	}

	var out []chunkSpec
	var cur []chunkSegment
	curTokens := 0

	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, specOf(cur))
		// Carry the tail of this chunk into the next one, so a sentence that
		// straddles a boundary is retrievable from either side. The carry
		// never exceeds `overlap`, which is what bounds a chunk at
		// target+overlap tokens.
		var carry []chunkSegment
		carried := 0
		for i := len(cur) - 1; i >= 0; i-- {
			if carried+cur[i].tokens > overlap {
				break
			}
			carry = append([]chunkSegment{cur[i]}, carry...)
			carried += cur[i].tokens
		}
		cur = carry
		curTokens = carried
	}

	for _, seg := range segments {
		if curTokens > 0 && curTokens+seg.tokens > target {
			flush()
		}
		cur = append(cur, seg)
		curTokens += seg.tokens
	}
	if curTokens > 0 {
		out = append(out, specOf(cur))
	}
	return out
}

// specOf assembles one chunk from its segments.
func specOf(segs []chunkSegment) chunkSpec {
	parts := make([]string, 0, len(segs))
	spec := chunkSpec{PageFrom: segs[0].page, PageTo: segs[0].page, StartPage: segs[0].page, StartOffset: segs[0].start}
	for _, s := range segs {
		parts = append(parts, s.text)
		spec.Tokens += s.tokens
		if s.page < spec.PageFrom {
			spec.PageFrom = s.page
		}
		if s.page > spec.PageTo {
			spec.PageTo = s.page
		}
	}
	spec.Text = strings.Join(parts, "\n\n")
	return spec
}

// segmentPages turns pages into paragraph segments, splitting any paragraph
// that is on its own larger than one chunk.
func segmentPages(pages []ingestPage, target int) []chunkSegment {
	var out []chunkSegment
	for _, page := range pages {
		base := 0 // code points of the page before this paragraph
		for _, raw := range strings.Split(page.Text, "\n\n") {
			at := base
			base += utf8.RuneCountInString(raw) + 2 // the "\n\n" it was split on
			para := strings.TrimSpace(collapseBlankLines(raw))
			if para == "" {
				continue
			}
			words := wordSpans(raw)
			tokens := estimateTokens(para)
			if tokens <= target {
				out = append(out, chunkSegment{text: para, page: page.Number, tokens: tokens,
					start: at + words[0][0], end: at + words[len(words)-1][1]})
				continue
			}
			for _, part := range splitLongParagraphAt(raw, words, target) {
				out = append(out, chunkSegment{text: part.text, page: page.Number, tokens: estimateTokens(part.text),
					start: at + part.start, end: at + part.end})
			}
		}
	}
	return out
}

// wordSpans lists the half-open code-point ranges of s's words — the same
// words strings.Fields finds.
func wordSpans(s string) [][2]int {
	var out [][2]int
	start, i := -1, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
		i++
	}
	if start >= 0 {
		out = append(out, [2]int{start, i})
	}
	return out
}

// longPart is one cut of an oversized paragraph: its words joined by single
// spaces, and the source range they span.
type longPart struct {
	text       string
	start, end int
}

// splitLongParagraphAt cuts an oversized paragraph on word boundaries. A
// table extracted with `pdftotext -layout` can be one enormous "paragraph";
// it still has to fit in a chunk. Each part keeps the source range of its
// words.
func splitLongParagraphAt(s string, words [][2]int, target int) []longPart {
	if len(words) == 0 {
		return nil
	}
	r := []rune(s)
	// Measured in characters, not per-word token estimates: summing an
	// estimate word by word drifts from the estimate of the joined string,
	// and the parts would come back over budget.
	maxChars := target * chunkCharsPerToken
	var out []longPart
	first, curChars := 0, 0
	flush := func(last int) {
		parts := make([]string, 0, last-first+1)
		for _, w := range words[first : last+1] {
			parts = append(parts, string(r[w[0]:w[1]]))
		}
		out = append(out, longPart{text: strings.Join(parts, " "), start: words[first][0], end: words[last][1]})
	}
	for i, w := range words {
		n := w[1] - w[0]
		if curChars > 0 && curChars+1+n > maxChars {
			flush(i - 1)
			first, curChars = i, 0
		}
		if curChars > 0 {
			curChars++ // the joining space
		}
		curChars += n
	}
	flush(len(words) - 1)
	return out
}

// collapseBlankLines normalises the ragged whitespace `pdftotext -layout`
// produces, so paragraph splitting is stable across pages.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// estimateTokens approximates the token count of a string. See
// chunkCharsPerToken for why an estimate is the right tool here.
func estimateTokens(s string) int {
	n := len([]rune(s)) / chunkCharsPerToken
	if n < 1 {
		return 1
	}
	return n
}

// jsonUnmarshalStepOutput decodes a step's jsonb output. It exists so the
// callers stay readable and so an empty output is never a parse error.
func jsonUnmarshalStepOutput(raw types.JSONB, dest any) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty step output")
	}
	return json.Unmarshal([]byte(raw), dest)
}
