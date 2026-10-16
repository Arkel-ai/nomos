package services

// OCR benchmark — opt-in, real gateway, real money (well under a dollar):
//
//	NOMOS_OCR_BENCH=1 go test ./services -run TestOCRBench -v -timeout 60m
//
// It builds a scanned bundle out of the testdata dossiers — fictitious
// documents. Their text-layer pages are rasterised to grey JPEG and rewrapped
// as image-only pages, so the source's own text layer is the ground truth, and
// blank separator sheets are slipped in the way a real "pièces 1 à 5" scan
// has them. Every configuration reads the whole bundle through the production
// path (cutPDF + visionReadPDF) with the llm pool's concurrency; tesseract
// reads it page by page, as the ocr pool does.
//
// Knobs: NOMOS_OCR_BENCH_MODELS (comma list), NOMOS_OCR_BENCH_RANGES (comma
// list of range sizes), NOMOS_OCR_BENCH_PAGES (text pages, default 48),
// NOMOS_OCR_BENCH_CONCURRENCY (default 2, production's llm pool),
// NOMOS_OCR_BENCH_TESSERACT=0 to skip the local baseline, and
// NOMOS_OCR_BENCH_OUT=<file> to write the markdown report.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	u "github.com/Arkel-ai/nomos/api/utils"
)

type benchPage struct {
	truth string // "" for a blank sheet
	blank bool
}

type benchConfig struct {
	name      string
	model     string // "" = tesseract
	rangeSize int
	reasoning string
}

type benchResult struct {
	cfg        benchConfig
	wall       time.Duration
	calls      int
	callTimes  []time.Duration
	costUSD    float64
	outTokens  int
	failures   map[string]int // why a range (or page) was lost
	missing    int            // pages the engine did not return
	blankOK    int
	blankTotal int
	recall     float64 // mean word recall over text pages
	precision  float64 // mean word precision over text pages
	cer        float64 // mean character error rate over text pages
}

// TestOCRBench is an optional benchmark, never part of a gate: it is excluded
// (a named skip, not a pass) unless NOMOS_OCR_BENCH=1, and always under
// NOMOS_LIVE=0.
func TestOCRBench(t *testing.T) {
	testmode.Exclude(t, "OCR benchmark (real gateway, costs money)", "NOMOS_OCR_BENCH")
	testmode.LoadDotenv("../.env")
	requireScanTools(t)
	if !u.LLMConfigured() {
		t.Skip("OPENROUTER_API_KEY is not set")
	}

	pdf, pages := benchBundle(t, benchEnvInt("NOMOS_OCR_BENCH_PAGES", 48))
	t.Logf("bundle: %d pages (%d blank), %d KB", len(pages), countBlank(pages), len(pdf)>>10)

	var configs []benchConfig
	if os.Getenv("NOMOS_OCR_BENCH_TESSERACT") != "0" {
		configs = append(configs, benchConfig{name: "tesseract (local, 1 page/step)"})
	}
	models := benchEnvList("NOMOS_OCR_BENCH_MODELS", "google/gemini-3.8-flash,anthropic/claude-haiku-5.5")
	for _, m := range models {
		for _, n := range benchEnvList("NOMOS_OCR_BENCH_RANGES", "1,4,8,16") {
			size, _ := strconv.Atoi(n)
			configs = append(configs, benchConfig{name: fmt.Sprintf("%s · %d p/call · low", m, size), model: m, rangeSize: size, reasoning: "low"})
		}
		configs = append(configs, benchConfig{name: fmt.Sprintf("%s · 8 p/call · default reasoning", m), model: m, rangeSize: 8})
	}

	concurrency := benchEnvInt("NOMOS_OCR_BENCH_CONCURRENCY", 2)
	var results []benchResult
	for _, cfg := range configs {
		t.Logf("running %s", cfg.name)
		var r benchResult
		if cfg.model == "" {
			r = benchTesseract(t, pdf, pages)
		} else {
			r = benchVision(t, cfg, pdf, pages, concurrency)
		}
		r.cfg = cfg
		results = append(results, r)
		t.Logf("  wall=%s cost=$%.4f missing=%d blank=%d/%d recall=%.3f cer=%.3f failures=%v",
			r.wall.Round(time.Second), r.costUSD, r.missing, r.blankOK, r.blankTotal, r.recall, r.cer, r.failures)
	}

	report := benchReport(results, len(pages), concurrency)
	t.Log("\n" + report)
	if out := os.Getenv("NOMOS_OCR_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
			t.Errorf("write report: %v", err)
		}
	}
}

// benchBundle rasterises the first `textPages` text-layer pages of the
// testdata dossiers and inserts a blank sheet every ~16 pages.
func benchBundle(t *testing.T, textPages int) ([]byte, []benchPage) {
	t.Helper()
	sources, err := filepath.Glob("../../testdata/dossier-*/*.pdf")
	if err != nil || len(sources) == 0 {
		t.Skip("no testdata dossiers to build the bundle from")
	}
	sort.Strings(sources)

	var jpgs [][]byte
	var pages []benchPage
	for _, src := range sources {
		if len(pages)-countBlank(pages) >= textPages {
			break
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		n := pdfPageCount(t, src)
		for p := 1; p <= n && len(pages)-countBlank(pages) < textPages; p++ {
			truth := pdftotextPage(t, src, p)
			if len(strings.TrimSpace(truth)) < 200 {
				continue // a scan already, or a near-empty page: no ground truth
			}
			jpgs = append(jpgs, rasteriseJPEG(t, raw, p, true))
			pages = append(pages, benchPage{truth: truth})
			if (len(pages)-countBlank(pages))%16 == 7 {
				jpgs = append(jpgs, blankPageJPEG(t))
				pages = append(pages, benchPage{blank: true})
			}
		}
	}
	return testImagePDF(t, jpgs...), pages
}

func benchVision(t *testing.T, cfg benchConfig, pdf []byte, pages []benchPage, concurrency int) benchResult {
	prev := ocrReasoningEffort
	ocrReasoningEffort = cfg.reasoning
	defer func() { ocrReasoningEffort = prev }()

	type rng struct{ from, to int }
	var ranges []rng
	for from := 1; from <= len(pages); from += cfg.rangeSize {
		ranges = append(ranges, rng{from, min(from+cfg.rangeSize-1, len(pages))})
	}

	got := make(map[int]string, len(pages))
	r := benchResult{failures: map[string]int{}}
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	started := time.Now()
	for _, rg := range ranges {
		wg.Add(1)
		sem <- struct{}{}
		go func(rg rng) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx := context.Background()
			part, err := cutPDF(ctx, pdf, rg.from, rg.to)
			if err != nil {
				mu.Lock()
				r.failures["cut_failed"]++
				mu.Unlock()
				return
			}
			callStart := time.Now()
			read, res, err := visionReadPDF(ctx, u.ChatOpts{Model: cfg.model}, part, rg.to-rg.from+1)
			elapsed := time.Since(callStart)
			mu.Lock()
			defer mu.Unlock()
			r.calls++
			r.callTimes = append(r.callTimes, elapsed)
			r.costUSD += res.CostUSD
			r.outTokens += res.CompletionTokens
			if err != nil {
				r.failures[visionFailure(err)]++
			}
			for k, text := range read {
				got[rg.from+k-1] = text
			}
		}(rg)
	}
	wg.Wait()
	r.wall = time.Since(started)
	scoreBench(&r, pages, got)
	return r
}

func benchTesseract(t *testing.T, pdf []byte, pages []benchPage) benchResult {
	requireTool(t, "tesseract")
	requireOCRLanguage(t)
	got := make(map[int]string, len(pages))
	r := benchResult{failures: map[string]int{}}
	started := time.Now()
	for p := 1; p <= len(pages); p++ {
		callStart := time.Now()
		image, err := rasterisePage(context.Background(), pdf, p)
		if err == nil {
			var text string
			text, _, err = tesseractOCR(context.Background(), image)
			got[p] = text
		}
		if err != nil {
			r.failures["tesseract_failed"]++
		}
		r.calls++
		r.callTimes = append(r.callTimes, time.Since(callStart))
	}
	r.wall = time.Since(started)
	scoreBench(&r, pages, got)
	return r
}

// scoreBench compares each page to its ground truth. Text pages get word
// recall/precision (order-insensitive, so a table read row-wise is not
// punished) and a character error rate; blank sheets must come back empty.
func scoreBench(r *benchResult, pages []benchPage, got map[int]string) {
	var textPages int
	for i, p := range pages {
		text, ok := got[i+1]
		if !ok {
			r.missing++
		}
		if p.blank {
			r.blankTotal++
			if ok && len(strings.TrimSpace(text)) < 10 {
				r.blankOK++
			}
			continue
		}
		textPages++
		rec, prec := wordOverlap(p.truth, text)
		r.recall += rec
		r.precision += prec
		r.cer += charErrorRate(p.truth, text)
	}
	if textPages > 0 {
		r.recall /= float64(textPages)
		r.precision /= float64(textPages)
		r.cer /= float64(textPages)
	}
}

func benchWords(s string) map[string]int {
	out := map[string]int{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[w]++
	}
	return out
}

func wordOverlap(truth, got string) (recall, precision float64) {
	tw, gw := benchWords(truth), benchWords(got)
	var common, nt, ng int
	for w, c := range tw {
		nt += c
		common += min(c, gw[w])
	}
	for _, c := range gw {
		ng += c
	}
	if nt > 0 {
		recall = float64(common) / float64(nt)
	}
	if ng > 0 {
		precision = float64(common) / float64(ng)
	}
	return recall, precision
}

// charErrorRate is the Levenshtein distance between the whitespace-collapsed,
// lower-cased texts, over the truth's length (capped at 1).
func charErrorRate(truth, got string) float64 {
	norm := func(s string) []rune {
		s = strings.NewReplacer("’", "'", "«", "\"", "»", "\"", "|", " ", "[signature]", " ").Replace(strings.ToLower(s))
		return []rune(strings.Join(strings.Fields(s), " "))
	}
	a, b := norm(truth), norm(got)
	if len(a) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return min(1, float64(prev[len(b)])/float64(len(a)))
}

func benchReport(results []benchResult, pages, concurrency int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OCR bench — %d-page scanned bundle, concurrency %d\n\n", pages, concurrency)
	b.WriteString("| configuration | wall | s/page | calls | mean call | max call | cost $ | $/100 p | pages lost | blank ok | word recall | word precision | CER | failures |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		var sum, max time.Duration
		for _, d := range r.callTimes {
			sum += d
			if d > max {
				max = d
			}
		}
		mean := time.Duration(0)
		if len(r.callTimes) > 0 {
			mean = sum / time.Duration(len(r.callTimes))
		}
		fails := []string{}
		for k, v := range r.failures {
			fails = append(fails, fmt.Sprintf("%s×%d", k, v))
		}
		sort.Strings(fails)
		fmt.Fprintf(&b, "| %s | %s | %.1f | %d | %s | %s | %.4f | %.3f | %d | %d/%d | %.3f | %.3f | %.3f | %s |\n",
			r.cfg.name, r.wall.Round(time.Second), r.wall.Seconds()/float64(pages), r.calls,
			mean.Round(100*time.Millisecond), max.Round(100*time.Millisecond), r.costUSD, r.costUSD*100/float64(pages),
			r.missing, r.blankOK, r.blankTotal, r.recall, r.precision, r.cer, strings.Join(fails, " "))
	}
	return b.String()
}

func pdfPageCount(t *testing.T, path string) int {
	out, err := exec.Command("pdfinfo", path).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			return n
		}
	}
	return 0
}

func pdftotextPage(t *testing.T, path string, page int) string {
	p := strconv.Itoa(page)
	out, err := exec.Command("pdftotext", "-enc", "UTF-8", "-f", p, "-l", p, path, "-").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func countBlank(pages []benchPage) int {
	n := 0
	for _, p := range pages {
		if p.blank {
			n++
		}
	}
	return n
}

func benchEnvInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func benchEnvList(key, def string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		v = def
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
