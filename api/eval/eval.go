package eval

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/fixtures"
	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var log = u.NewLogger("eval")

// ---------------------------------------------------------------------------
// The query set
// ---------------------------------------------------------------------------

//go:embed testcases/*.json
var testcaseFS embed.FS

// Gold is one relevant article and how relevant it is. Article is the bare
// Code du travail number as the fixture corpus writes it — "L1234-9",
// "R1455-7", "L1453-1A" — never a citation label, because the label format is
// a presentation choice and the gold set must survive it changing.
type Gold struct {
	Article string `json:"article"`
	Grade   int    `json:"grade"`
}

// TestCase is one query and what a French employment lawyer would expect back.
//
// The four "expectation" fields are deliberately separate, because they fail
// for different reasons and must never be traded off against each other:
//
//	Gold      — must come back (recall / nDCG / MRR / precision).
//	Forbidden — must NOT come back, at any rank. Temporal cases: an article not
//	            in force at the as-of date is wrong law, not a bad rank (§6.4).
//	NearMiss  — may come back, but must not OUTRANK the primary gold article.
//	            This is the failure recall cannot see.
//	(empty Gold + category negative_control) — nothing may cross the relevance
//	            threshold. The abstention case.
type TestCase struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags,omitempty"`
	Query     string   `json:"query"`
	Gold      []Gold   `json:"gold,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
	NearMiss  []string `json:"near_miss,omitempty"`
	// AsOf is the date of the FACTS, "YYYY-MM-DD". Empty means today, which
	// is only correct for a current dispute (§6.4).
	AsOf string `json:"as_of,omitempty"`
	// VariantOf names the reference case this one is an accent/casing variant
	// of. Both must return the same articles in the same order.
	VariantOf string `json:"variant_of,omitempty"`
	// MinScore overrides the per-strategy abstention threshold for this case.
	MinScore float64 `json:"min_score,omitempty"`
	Note     string  `json:"note"`

	// set is the file the case came from; filled in by LoadSuite.
	set string
}

// Categories every suite must cover. Listed here rather than in a test so the
// requirement is visible where the cases are defined.
var RequiredCategories = []string{
	"droit_du_travail", "procedure", "contrat", "rupture", "negative_control", "temporal",
}

type caseFile struct {
	Set   string     `json:"set"`
	Note  string     `json:"note"`
	Cases []TestCase `json:"cases"`
}

// LoadSuite reads and validates every embedded case file.
//
// Validation is not a formality: a gold article that does not exist in the
// corpus silently caps recall below 1.0 for every strategy at once, which
// looks exactly like a retrieval regression. It is checked against the
// fixture package, so this runs with no database and no API key.
func LoadSuite() ([]TestCase, error) {
	entries, err := fs.Glob(testcaseFS, "testcases/*.json")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	var cases []TestCase
	for _, name := range entries {
		raw, err := testcaseFS.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var f caseFile
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for i := range f.Cases {
			f.Cases[i].set = strings.TrimSuffix(path.Base(name), ".json")
			cases = append(cases, f.Cases[i])
		}
	}
	if err := Validate(cases); err != nil {
		return nil, err
	}
	return cases, nil
}

// Validate is every structural rule the query set has to satisfy.
func Validate(cases []TestCase) error {
	known, err := corpusArticles()
	if err != nil {
		return err
	}
	byID := make(map[string]TestCase, len(cases))
	seenCategory := map[string]bool{}
	var problems []string

	for _, c := range cases {
		if _, dup := byID[c.ID]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate id", c.ID))
		}
		byID[c.ID] = c
		seenCategory[c.Category] = true

		if strings.TrimSpace(c.Query) == "" {
			problems = append(problems, c.ID+": empty query")
		}
		if strings.TrimSpace(c.Note) == "" {
			problems = append(problems, c.ID+": a case with no note is a case nobody can maintain")
		}
		if c.Category == "negative_control" && len(c.Gold) > 0 {
			problems = append(problems, c.ID+": a negative control cannot have gold articles")
		}
		if c.Category != "negative_control" && len(c.Gold) == 0 && len(c.Forbidden) == 0 {
			problems = append(problems, c.ID+": no gold and no forbidden articles — this case asserts nothing")
		}
		if c.AsOf != "" {
			if _, err := time.Parse("2006-01-02", c.AsOf); err != nil {
				problems = append(problems, fmt.Sprintf("%s: as_of %q is not YYYY-MM-DD", c.ID, c.AsOf))
			}
		}
		for _, g := range c.Gold {
			if g.Grade < GradeSupporting || g.Grade > maxGrade {
				problems = append(problems, fmt.Sprintf("%s: grade %d for %s is outside 1..%d", c.ID, g.Grade, g.Article, maxGrade))
			}
			if !known[g.Article] {
				problems = append(problems, fmt.Sprintf("%s: gold article %s is not in the fixture corpus", c.ID, g.Article))
			}
		}
		// Forbidden and near-miss articles MUST exist too: an expectation
		// that an absent article does not come back is trivially satisfied
		// and proves nothing.
		for _, a := range append(append([]string{}, c.Forbidden...), c.NearMiss...) {
			if !known[a] {
				problems = append(problems, fmt.Sprintf("%s: article %s is not in the fixture corpus", c.ID, a))
			}
		}
	}

	for _, c := range cases {
		if c.VariantOf == "" {
			continue
		}
		ref, ok := byID[c.VariantOf]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: variant_of %q does not exist", c.ID, c.VariantOf))
			continue
		}
		if !sameGold(ref.Gold, c.Gold) {
			problems = append(problems, fmt.Sprintf("%s: variant gold differs from %s — a variant that expects something else is not a variant", c.ID, ref.ID))
		}
	}

	for _, want := range RequiredCategories {
		if !seenCategory[want] {
			problems = append(problems, "missing required category: "+want)
		}
	}
	if len(cases) < 40 {
		problems = append(problems, fmt.Sprintf("only %d cases; the harness commits to at least 40", len(cases)))
	}
	if len(problems) > 0 {
		return errors.New("testcase suite is invalid:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

func sameGold(a, b []Gold) bool {
	if len(a) != len(b) {
		return false
	}
	ma, mb := goldMap(a), goldMap(b)
	for k, v := range ma {
		if mb[k] != v {
			return false
		}
	}
	return true
}

func goldMap(gs []Gold) map[string]int {
	m := make(map[string]int, len(gs))
	for _, g := range gs {
		m[g.Article] = g.Grade
	}
	return m
}

// ---------------------------------------------------------------------------
// Article identity
// ---------------------------------------------------------------------------

// articleRe pulls the article number out of a citation label. Both the fixture
// form ("C. trav., art. L1234-9") and the spelled-out form ("Article L. 1234-9
// du code du travail") are accepted, because the label is presentation and the
// harness must not break when it is reworded.
var articleRe = regexp.MustCompile(`(?i)\b(?:art\.?|article)\s*([LRD])\.?\s*([0-9]+(?:-[0-9]+)*(?:-?[A-Z])?)`)

// ArticleOf returns the bare article number a hit belongs to, or "".
func ArticleOf(citationLabel string) string {
	m := articleRe.FindStringSubmatch(citationLabel)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + m[2]
}

// corpusArticles is the set of article numbers the fixture corpus contains.
// Read from the embedded fixture, NOT from the database: the query set must be
// verifiable in CI with no infrastructure at all.
func corpusArticles() (map[string]bool, error) {
	c, err := fixtureCorpus()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(c))
	for _, n := range c {
		out[n] = true
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Cost model
// ---------------------------------------------------------------------------

// Published list prices, in one place, clearly labelled as ESTIMATES. The
// audited figures live in llm_calls (§4.6); these exist so the
// scoreboard can answer "does the reranker earn its money" without a billing
// export. Both were verified against OpenRouter's model pages in §5.1 / §6.2.
const (
	embedPriceUSDPerMTok        = 0.02  // openai/text-embedding-3-small
	rerankPriceUSDPerSearchUnit = 0.002 // cohere/rerank-4-fast
	// A rerank search unit is one query with up to 100 documents; a document
	// longer than 500 tokens is split and counted more than once.
	rerankTokensPerDoc = 500
	rerankDocsPerUnit  = 100
	estCharsPerToken   = 4 // matches services/corpus.go — a shape, not billing
)

// estimateCost is the modelled USD cost of ONE query under one strategy.
func estimateCost(strategy string, queryChars int, candidateChars []int) float64 {
	cost := 0.0
	if strategy != services.StrategyFTS {
		tok := float64(queryChars) / estCharsPerToken
		cost += tok / 1e6 * embedPriceUSDPerMTok
	}
	if strategy == services.StrategyRRFRerank && len(candidateChars) > 0 {
		docUnits := 0.0
		for _, n := range candidateChars {
			docUnits += math.Ceil(float64(n) / estCharsPerToken / rerankTokensPerDoc)
		}
		units := math.Ceil(docUnits / rerankDocsPerUnit)
		if units < 1 {
			units = 1
		}
		cost += units * rerankPriceUSDPerSearchUnit
	}
	return cost
}

// ---------------------------------------------------------------------------
// Running one configuration
// ---------------------------------------------------------------------------

// RunOptions is what the harness itself needs, as opposed to what retrieval
// needs. Source exists because the shared dev database also holds other
// packages' seeded rows: scoring against another test's corpus measures that
// test, not this one.
type RunOptions struct {
	Strategy string
	// Source restricts legal_documents.source. "fixtures" is the committed
	// offline corpus this query set is labelled against.
	Source string
	// TopK is the cut the metrics are computed over. 10 mirrors §6.2.
	TopK int
	// States is legal_versions.state to accept. Empty means "whatever this
	// corpus source actually writes", resolved by CorpusStates.
	//
	// This knob exists because of a live defect, not because state is an
	// interesting dimension to sweep: the fixture loader writes LEGI's own
	// vocabulary ("vigueur") while retrieval defaults to "active", so the
	// SHIPPED default configuration returns zero hits against the committed
	// corpus. Preflight names it; the harness compensates so there is a
	// scoreboard at all. See README.md, weakness 1.
	States []string
	// Timeout bounds one query.
	Timeout time.Duration
}

func (o RunOptions) normalized() RunOptions {
	if o.Strategy == "" {
		o.Strategy = services.StrategyRRFRerank
	}
	if o.Source == "" {
		o.Source = services.SourceFixtures
	}
	if o.TopK <= 0 {
		o.TopK = 10
	}
	if o.Timeout <= 0 {
		o.Timeout = 90 * time.Second
	}
	if len(o.States) == 0 {
		o.States = CorpusStates(o.Source)
	}
	return o
}

// CorpusStates reports the distinct legal_versions.state values a corpus
// source actually uses. Nil when the database is unreachable, which leaves
// retrieval on its own default.
func CorpusStates(source string) []string {
	if u.DB == nil {
		return nil
	}
	var states []string
	err := u.DB.Table("legal_versions v").
		Joins("JOIN legal_documents d ON d.id = v.legal_document_id").
		Where("d.source = ?", source).
		Distinct().Pluck("v.state", &states).Error
	if err != nil {
		log.Warnf("Could not read corpus states (source=%s): %v", source, err)
		return nil
	}
	sort.Strings(states)
	return states
}

// CaseResult is one query under one strategy: what came back, how fast, and
// what it modelled as costing.
type CaseResult struct {
	Case     TestCase
	Articles []string // top-k article numbers, rank order, deduplicated
	Scores   []float64
	Reranked bool
	Latency  time.Duration
	CostUSD  float64
	Err      error
}

// RunCase scores one test case. It calls services.Retrieve — the same entry
// point POST /api/search uses — on purpose.
func RunCase(ctx context.Context, tc TestCase, opts RunOptions) CaseResult {
	opts = opts.normalized()
	res := CaseResult{Case: tc}

	ropts := services.DefaultRetrievalOptions()
	ropts.Strategy = opts.Strategy
	ropts.TopK = opts.TopK
	ropts.Sources = []string{opts.Source}
	if len(opts.States) > 0 {
		ropts.States = opts.States
	}
	if tc.AsOf != "" {
		d, err := time.Parse("2006-01-02", tc.AsOf)
		if err != nil {
			res.Err = fmt.Errorf("as_of: %w", err)
			return res
		}
		ropts.AsOfDate = d.UTC().Unix()
	}

	qctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	start := time.Now()
	out, err := services.Retrieve(qctx, services.RetrievalRequest{
		Query:   tc.Query,
		Corpus:  types.CorpusPublic,
		Options: ropts,
	})
	res.Latency = time.Since(start)
	if err != nil {
		res.Err = err
		return res
	}
	res.Reranked = out.Reranked

	chars := make([]int, 0, len(out.Hits))
	for _, h := range out.Hits {
		a := ArticleOf(h.CitationLabel)
		if a == "" {
			a = ArticleOf(h.Text)
		}
		if a == "" {
			// A hit whose article cannot be identified is a scoring hole,
			// not a silent miss: name it so it shows up as a defect.
			a = fmt.Sprintf("?chunk-%d", h.ChunkID)
		}
		res.Articles = append(res.Articles, a)
		res.Scores = append(res.Scores, h.Score)
		chars = append(chars, len(h.Text))
	}
	res.CostUSD = estimateCost(opts.Strategy, len(tc.Query), chars)
	return res
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// Metrics is one aggregated scorecard. Every field is a macro-average over
// queries: one query, one vote, regardless of how many gold articles it has.
type Metrics struct {
	Queries    int
	RecallAt1  float64
	RecallAt5  float64
	RecallAt10 float64
	RecallAt20 float64
	MRR        float64
	NDCGAt10   float64
	PrecAt5    float64
}

// Report is one strategy scored over the whole suite.
type Report struct {
	Strategy string
	Source   string
	TopK     int
	States   []string

	Overall    Metrics
	ByCategory map[string]Metrics

	// Abstention over the negative controls, at AbstentionThreshold.
	NegativeControls     int
	Abstained            int
	AbstentionRate       float64
	AbstentionCalibrated bool
	Threshold            float64
	// AbstainDetail names the negative controls the system answered anyway,
	// with what it answered. The rate is the number; this is the diagnosis.
	AbstainDetail []string

	// Temporal leakage: cases carrying a Forbidden list, and how many leaked.
	TemporalCases int
	TemporalLeaks int
	LeakDetail    []string

	// Near-miss inversions: an impostor ranked above the primary gold article.
	NearMissCases      int
	NearMissInversions int
	InversionDetail    []string

	// Accent/casing variant agreement against the reference case.
	VariantPairs    int
	VariantAgreeAvg float64

	Errors []string

	LatencyP50  time.Duration
	LatencyP95  time.Duration
	MeanCostUSD float64
	Results     []CaseResult
}

// abstentionThreshold is the score above which a hit counts as "the system
// says this is relevant".
//
// Only rrf_rerank has a CALIBRATED score: cohere/rerank-4-fast returns a 0–1
// relevance estimate. An RRF score is a sum of 1/(60+rank) and expresses rank,
// not relevance — its top hit is ~0.0328 whether the corpus answers the
// question or not, so any threshold over it is arbitrary. The harness reports
// the rate anyway, marked uncalibrated, because "this strategy structurally
// cannot abstain" is itself the finding (README.md).
//
// 0.60 IS MODEL-SPECIFIC AND MUST BE RE-MEASURED WHEN THE RERANKER CHANGES.
// It was 0.30 for cohere/rerank-v3.5. rerank-4-fast scores the same documents
// higher across the board, so 0.30 landed in the middle of the negative-control
// distribution and abstention collapsed from 0.75 to 0.42 — the system got more
// confident about questions it cannot answer, which is the exact failure the
// negative controls exist to catch.
//
// Measured over the 106-case suite against the fixture corpus, 2026-08-28:
//
//	negative controls (n=12): min 0.1215  p50 0.3087  p75 0.5904  max 0.8896
//	graded queries    (n=88): min 0.6312  p10 0.8251  p50 0.9160  max 0.9697
//
// The two populations separate cleanly, so the threshold is chosen as the
// highest value that suppresses NO graded query — the graded minimum is 0.6312,
// which puts the ceiling just under it. 0.60 restores abstention to 0.75 at zero
// cost to recall. 0.70 would abstain more (0.83) but starts suppressing a real
// answer, and a legal tool that hides an article it did find is worse than one
// that shows a weak one.
func abstentionThreshold(strategy string) (float64, bool) {
	switch strategy {
	case services.StrategyRRFRerank:
		return 0.60, true
	default:
		// Above the RRF score of a hit that appeared in exactly one arm at
		// rank 1 (1/61 ≈ 0.0164) — i.e. "both arms agree it is near the top".
		return 0.0250, false
	}
}

// Aggregate turns per-case results into the scorecard.
func Aggregate(strategy, source string, topK int, states []string, results []CaseResult) Report {
	threshold, calibrated := abstentionThreshold(strategy)
	rep := Report{
		Strategy:             strategy,
		Source:               source,
		TopK:                 topK,
		States:               states,
		ByCategory:           map[string]Metrics{},
		Threshold:            threshold,
		AbstentionCalibrated: calibrated,
		Results:              results,
	}

	overall := &acc{}
	byCat := map[string]*acc{}
	byID := map[string]CaseResult{}

	var latencies []time.Duration
	var costs []float64

	for _, r := range results {
		byID[r.Case.ID] = r
		if r.Err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", r.Case.ID, r.Err))
			continue
		}
		latencies = append(latencies, r.Latency)
		costs = append(costs, r.CostUSD)

		if len(r.Case.Gold) > 0 {
			gold := goldMap(r.Case.Gold)
			a, ok := byCat[r.Case.Category]
			if !ok {
				a = &acc{}
				byCat[r.Case.Category] = a
			}
			for _, dst := range []*acc{overall, a} {
				dst.r1 = append(dst.r1, RecallAtK(r.Articles, gold, 1))
				dst.r5 = append(dst.r5, RecallAtK(r.Articles, gold, 5))
				dst.r10 = append(dst.r10, RecallAtK(r.Articles, gold, 10))
				dst.r20 = append(dst.r20, RecallAtK(r.Articles, gold, 20))
				dst.mrr = append(dst.mrr, ReciprocalRank(r.Articles, gold))
				dst.ndcg = append(dst.ndcg, NDCGAtK(r.Articles, gold, 10))
				dst.p5 = append(dst.p5, PrecisionAtK(r.Articles, gold, 5))
			}
		}

		if r.Case.Category == "negative_control" {
			rep.NegativeControls++
			th := threshold
			if r.Case.MinScore > 0 {
				th = r.Case.MinScore
			}
			if Abstained(r.Scores, th) {
				rep.Abstained++
			} else if len(r.Articles) > 0 {
				rep.AbstainDetail = append(rep.AbstainDetail,
					fmt.Sprintf("%s answered with %s (score %.3f >= %.2f): %s",
						r.Case.ID, r.Articles[0], r.Scores[0], th, r.Case.Query))
			}
		}

		if len(r.Case.Forbidden) > 0 {
			rep.TemporalCases++
			if leaked := LeakedForbidden(r.Articles, r.Case.Forbidden, topK); len(leaked) > 0 {
				rep.TemporalLeaks++
				rep.LeakDetail = append(rep.LeakDetail,
					fmt.Sprintf("%s (as_of %s) leaked %s", r.Case.ID, r.Case.AsOf, strings.Join(leaked, ", ")))
			}
		}

		if len(r.Case.NearMiss) > 0 && len(r.Case.Gold) > 0 {
			rep.NearMissCases++
			if imp, inverted := firstInversion(r.Articles, r.Case); inverted {
				rep.NearMissInversions++
				rep.InversionDetail = append(rep.InversionDetail,
					fmt.Sprintf("%s: %s ranked above the primary gold article", r.Case.ID, imp))
			}
		}
	}

	// Variant agreement, once both halves of a pair have run.
	var agreements []float64
	for _, r := range results {
		if r.Case.VariantOf == "" || r.Err != nil {
			continue
		}
		ref, ok := byID[r.Case.VariantOf]
		if !ok || ref.Err != nil {
			continue
		}
		rep.VariantPairs++
		agreements = append(agreements, RankAgreement(ref.Articles, r.Articles, topK))
	}
	rep.VariantAgreeAvg = Mean(agreements)

	rep.Overall = summarize(overall)
	for cat, a := range byCat {
		rep.ByCategory[cat] = summarize(a)
	}
	if rep.NegativeControls > 0 {
		rep.AbstentionRate = float64(rep.Abstained) / float64(rep.NegativeControls)
	}
	rep.LatencyP50, rep.LatencyP95 = percentiles(latencies)
	rep.MeanCostUSD = Mean(costs)
	return rep
}

// acc collects the per-query scores a Metrics is the mean of.
type acc struct{ r1, r5, r10, r20, mrr, ndcg, p5 []float64 }

func summarize(a *acc) Metrics {
	return Metrics{
		Queries:    len(a.ndcg),
		RecallAt1:  Mean(a.r1),
		RecallAt5:  Mean(a.r5),
		RecallAt10: Mean(a.r10),
		RecallAt20: Mean(a.r20),
		MRR:        Mean(a.mrr),
		NDCGAt10:   Mean(a.ndcg),
		PrecAt5:    Mean(a.p5),
	}
}

// firstInversion reports whether a near-miss article outranks EVERY primary
// gold article. Ranking an impostor above one primary but below another is not
// an inversion — the lawyer still reads the right rule first.
func firstInversion(ranked []string, tc TestCase) (string, bool) {
	primary := map[string]bool{}
	for _, g := range tc.Gold {
		if g.Grade >= GradePrimary {
			primary[g.Article] = true
		}
	}
	if len(primary) == 0 {
		return "", false
	}
	impostors := map[string]bool{}
	for _, a := range tc.NearMiss {
		impostors[a] = true
	}
	for _, a := range dedupe(ranked) {
		if impostors[a] {
			return a, true
		}
		if primary[a] {
			return "", false
		}
	}
	return "", false
}

func percentiles(ds []time.Duration) (p50, p95 time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	s := append([]time.Duration{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := func(q float64) int {
		i := int(math.Ceil(q*float64(len(s)))) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(s) {
			i = len(s) - 1
		}
		return i
	}
	return s[idx(0.50)], s[idx(0.95)]
}

// RunSuite scores one strategy over every case, sequentially.
//
// Sequential on purpose: the scoreboard reports wall-clock latency per query,
// and a concurrent runner would measure the harness's parallelism instead of
// the retrieval path's cost.
func RunSuite(ctx context.Context, cases []TestCase, opts RunOptions) Report {
	opts = opts.normalized()
	results := make([]CaseResult, 0, len(cases))
	for _, tc := range cases {
		if ctx.Err() != nil {
			break
		}
		results = append(results, RunCase(ctx, tc, opts))
	}
	rep := Aggregate(opts.Strategy, opts.Source, opts.TopK, opts.States, results)
	log.Infof("Suite scored (strategy=%s cases=%d ndcg@10=%.3f recall@10=%.3f abstention=%.2f p50=%s)",
		opts.Strategy, len(results), rep.Overall.NDCGAt10, rep.Overall.RecallAt10,
		rep.AbstentionRate, rep.LatencyP50.Round(time.Millisecond))
	return rep
}

// ---------------------------------------------------------------------------
// Preconditions — skip loudly, never fake a score
// ---------------------------------------------------------------------------

// ErrNoCorpus / ErrNoGateway are the two "cannot measure" conditions. They are
// returned, never swallowed: a harness that silently reports 0.000 because the
// corpus is empty is worse than one that does not run.
var (
	ErrNoCorpus  = errors.New("eval: the fixture corpus is not loaded — run the corpus sync first")
	ErrNoGateway = errors.New("eval: OPENROUTER_API_KEY is not set — embeddings and rerank cannot run")
	ErrNoDB      = errors.New("eval: DATABASE_URL is not set — there is nothing to retrieve from")
)

// Preflight loads api/.env, initialises the clients and verifies that there is
// something real to measure. It returns a human-readable reason on failure so
// the caller can skip with a message instead of a stack trace.
func Preflight(source string) error {
	// One call per candidate, NOT one call with three names: godotenv.Load
	// stops at the first file it cannot open, so a single call from the
	// package directory (where `go test` runs, and where there is no .env)
	// never reaches ../.env and the live tests skip on a machine that is in
	// fact fully configured — a silent "no signal" reported as green.
	for _, f := range []string{".env", "../.env", "../../.env"} {
		_ = godotenv.Load(f)
	}
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return ErrNoDB
	}
	if cfg.OpenRouterAPIKey == "" {
		return ErrNoGateway
	}
	if u.DB == nil {
		u.InitClients(cfg)
	}
	if u.DB == nil {
		return fmt.Errorf("%w (connection failed)", ErrNoDB)
	}
	if source == "" {
		source = services.SourceFixtures
	}

	var chunks int64
	err := u.DB.Table("legal_chunks c").
		Joins("JOIN legal_versions v ON v.id = c.legal_version_id").
		Joins("JOIN legal_documents d ON d.id = v.legal_document_id").
		Where("d.source = ? AND c.embedding IS NOT NULL", source).
		Count(&chunks).Error
	if err != nil {
		return fmt.Errorf("eval: counting corpus chunks: %w", err)
	}
	if chunks == 0 {
		return fmt.Errorf("%w (source=%s)", ErrNoCorpus, source)
	}
	// The state-vocabulary mismatch WAS a product defect: the loader wrote
	// LEGI's "vigueur" while retrieval filtered on "active", so the shipped
	// default returned zero hits against a fully loaded corpus. Both sides now
	// go through types.NormalizeLegalVersionState, and this check is the
	// tripwire that keeps them together — it compares what the corpus actually
	// contains against what retrieval actually asks for, rather than hardcoding
	// either. A harness that quietly works around a bug hides it, so say so
	// every single run if they ever drift apart again.
	states := CorpusStates(source)
	shipped := types.NormalizeLegalVersionState(types.LegalVersionInForce)
	if len(states) > 0 && !slices.Contains(states, shipped) {
		log.Errorf("DEFECT: source=%s writes legal_versions.state=%v but retrieval defaults to %q — "+
			"the SHIPPED default configuration returns zero hits against this corpus. "+
			"The harness scores with states=%v so there is a measurement at all; the product is not compensated.",
			source, states, shipped, states)
	}

	log.Infof("Preflight ok (source=%s embedded_chunks=%d states=%v model=%s rerank=%s)",
		source, chunks, states, cfg.EmbeddingModel, cfg.RerankModel)
	return nil
}

// fixtureCorpus returns every article number in the committed offline corpus.
// It reads api/fixtures directly rather than the database so the query set can
// be validated in CI with no Postgres and no credentials.
func fixtureCorpus() ([]string, error) {
	c, err := fixtures.CodeDuTravail()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(c.Articles))
	for _, a := range c.Articles {
		out = append(out, a.Number)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Scoreboard
// ---------------------------------------------------------------------------

// FormatScoreboard renders the comparison. It is the point of the package: a
// fusion that is not measurable against its own single arms is a claim, not a
// result, so every strategy is scored over the SAME query set and printed side
// by side with what it costs and how long it takes.
func FormatScoreboard(reports []Report) string {
	var b strings.Builder
	if len(reports) == 0 {
		return "no reports\n"
	}

	fmt.Fprintf(&b, "NOMOS RELEVANCE SCOREBOARD\n")
	fmt.Fprintf(&b, "corpus source=%s   states=%v   top_k=%d   cases=%d   %s\n\n",
		reports[0].Source, reports[0].States, reports[0].TopK, len(reports[0].Results),
		time.Now().UTC().Format("2006-01-02 15:04 UTC"))

	fmt.Fprintf(&b, "%-12s %7s %7s %7s %7s %7s %8s %7s %9s %9s %11s\n",
		"strategy", "R@1", "R@5", "R@10", "R@20", "MRR", "nDCG@10", "P@5", "p50", "p95", "USD/1k q")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 106))
	for _, r := range reports {
		m := r.Overall
		fmt.Fprintf(&b, "%-12s %7.3f %7.3f %7.3f %7.3f %7.3f %8.3f %7.3f %9s %9s %11.6f\n",
			r.Strategy, m.RecallAt1, m.RecallAt5, m.RecallAt10, m.RecallAt20,
			m.MRR, m.NDCGAt10, m.PrecAt5,
			r.LatencyP50.Round(time.Millisecond), r.LatencyP95.Round(time.Millisecond), r.MeanCostUSD*1000)
	}
	fmt.Fprintf(&b, "\n%d graded queries per strategy. USD/1k q is MODELLED from published list prices\n"+
		"(openai/text-embedding-3-small $%.2f/M tokens, cohere/rerank-4-fast $%.3f/search unit), not billed usage.\n",
		reports[0].Overall.Queries, embedPriceUSDPerMTok, rerankPriceUSDPerSearchUnit)

	// Per category.
	cats := map[string]bool{}
	for _, r := range reports {
		for c := range r.ByCategory {
			cats[c] = true
		}
	}
	ordered := make([]string, 0, len(cats))
	for c := range cats {
		ordered = append(ordered, c)
	}
	sort.Strings(ordered)

	fmt.Fprintf(&b, "\nPER CATEGORY — nDCG@10 / recall@10 (n = graded queries)\n")
	fmt.Fprintf(&b, "%-20s %4s", "category", "n")
	for _, r := range reports {
		fmt.Fprintf(&b, " %19s", r.Strategy)
	}
	b.WriteString("\n" + strings.Repeat("-", 24+20*len(reports)) + "\n")
	for _, c := range ordered {
		n := 0
		if m, ok := reports[0].ByCategory[c]; ok {
			n = m.Queries
		}
		fmt.Fprintf(&b, "%-20s %4d", c, n)
		for _, r := range reports {
			m := r.ByCategory[c]
			fmt.Fprintf(&b, " %9.3f /%8.3f", m.NDCGAt10, m.RecallAt10)
		}
		b.WriteString("\n")
	}

	// The checks recall cannot see.
	fmt.Fprintf(&b, "\nBEHAVIOURAL CHECKS\n")
	fmt.Fprintf(&b, "%-12s %26s %22s %22s %18s\n",
		"strategy", "abstention (neg ctrl)", "temporal leaks", "near-miss inversions", "variant agree")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 104))
	for _, r := range reports {
		// An uncalibrated strategy is reported as n/a, NOT as a percentage.
		// Its top hit scores 1/(60+1) = 0.0164 whether the corpus answers the
		// question or not, so it "abstains" on every query including the ones
		// it answered correctly. Printing 100% there would be a fabricated
		// success, which is the exact failure this whole file exists to catch.
		abst := "n/a (uncalibrated)"
		if r.AbstentionCalibrated {
			abst = fmt.Sprintf("%.0f%% (%d/%d)", r.AbstentionRate*100, r.Abstained, r.NegativeControls)
		}
		fmt.Fprintf(&b, "%-12s %26s %13d/%-8d %13d/%-8d %17.3f\n",
			r.Strategy, abst,
			r.TemporalLeaks, r.TemporalCases,
			r.NearMissInversions, r.NearMissCases,
			r.VariantAgreeAvg)
	}
	calThreshold, _ := abstentionThreshold(services.StrategyRRFRerank)
	uncalThreshold, _ := abstentionThreshold(services.StrategyRRF)
	fmt.Fprintf(&b, "\nabstention  = share of negative controls where NO hit scored at or above the threshold\n")
	fmt.Fprintf(&b, "              (rrf_rerank: %.2f on the cross-encoder's 0-1 relevance score. The other three\n", calThreshold)
	fmt.Fprintf(&b, "               have no calibrated score at all: an RRF score is a sum of 1/(60+rank), so the\n")
	fmt.Fprintf(&b, "               %.4f used for them is arbitrary and their column is not comparable — README.md)\n", uncalThreshold)
	fmt.Fprintf(&b, "temporal    = cases where an article NOT in force at the as-of date was returned\n")
	fmt.Fprintf(&b, "near-miss   = cases where a lexically similar but legally wrong article outranked\n")
	fmt.Fprintf(&b, "              every primary gold article\n")
	fmt.Fprintf(&b, "variant     = top-k slot-for-slot agreement between an accented query and its\n")
	fmt.Fprintf(&b, "              unaccented / upper-cased twin (1.000 = identical ordering)\n")

	for _, r := range reports {
		if len(r.LeakDetail) == 0 && len(r.InversionDetail) == 0 &&
			len(r.AbstainDetail) == 0 && len(r.Errors) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s — detail\n", r.Strategy)
		for _, d := range r.AbstainDetail {
			fmt.Fprintf(&b, "  NO ABSTENTION   %s\n", d)
		}
		for _, d := range r.LeakDetail {
			fmt.Fprintf(&b, "  TEMPORAL LEAK   %s\n", d)
		}
		for _, d := range r.InversionDetail {
			fmt.Fprintf(&b, "  NEAR-MISS       %s\n", d)
		}
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "  ERROR           %s\n", e)
		}
	}
	return b.String()
}

// FormatWorstCases lists the queries a strategy handled worst. The scoreboard
// says whether retrieval is good; this says where it is bad, which is the part
// that tells you what to fix.
func FormatWorstCases(rep Report, n int) string {
	type row struct {
		id, cat  string
		ndcg     float64
		query    string
		articles []string
	}
	var rows []row
	for _, r := range rep.Results {
		if r.Err != nil || len(r.Case.Gold) == 0 {
			continue
		}
		rows = append(rows, row{r.Case.ID, r.Case.Category,
			NDCGAtK(r.Articles, goldMap(r.Case.Gold), 10), r.Case.Query, r.Articles})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ndcg < rows[j].ndcg })
	if n < len(rows) {
		rows = rows[:n]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nWORST %d QUERIES — %s\n", len(rows), rep.Strategy)
	for _, r := range rows {
		top := r.articles
		if len(top) > 5 {
			top = top[:5]
		}
		fmt.Fprintf(&b, "  %-9s nDCG@10=%.3f  [%s]\n      %s\n      returned: %s\n",
			r.id, r.ndcg, r.cat, r.query, strings.Join(top, ", "))
	}
	return b.String()
}
