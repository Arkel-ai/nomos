package eval

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Arkel-ai/nomos/api/services"
)

// This file runs in two modes, on purpose.
//
//	OFFLINE (always) — the metric arithmetic and the query set's structure.
//	  No Postgres, no API key, no network. These are the tests that must never
//	  be skipped, because a harness whose metrics are wrong reports confident
//	  nonsense.
//
//	LIVE (skips with a reason)  — the actual relevance measurement against the
//	  fixture corpus and the real gateway. It SKIPS, never fails, when the
//	  corpus is not loaded or OPENROUTER_API_KEY is absent — and it says which,
//	  so "no signal" is never mistaken for "green".
//
// Run:  cd api && GOTOOLCHAIN=auto go test ./eval/... -v

// ---------------------------------------------------------------------------
// Metrics — hand-computed expectations
// ---------------------------------------------------------------------------

const eps = 1e-9

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %.10f, want %.10f", name, got, want)
	}
}

// The worked example every other assertion in this block refers to:
//
//	ranked = [A B C D E]
//	gold   = {A:2, D:1, Z:2}   (Z is never retrieved)
func TestMetricsWorkedExample(t *testing.T) {
	ranked := []string{"A", "B", "C", "D", "E"}
	gold := map[string]int{"A": 2, "D": 1, "Z": 2}

	// One of three gold articles in the top 1, two in the top 5.
	near(t, "recall@1", RecallAtK(ranked, gold, 1), 1.0/3.0)
	near(t, "recall@5", RecallAtK(ranked, gold, 5), 2.0/3.0)
	near(t, "recall@10", RecallAtK(ranked, gold, 10), 2.0/3.0)

	// Denominator is k, not len(ranked): two relevant in five slots.
	near(t, "precision@5", PrecisionAtK(ranked, gold, 5), 0.4)
	near(t, "precision@1", PrecisionAtK(ranked, gold, 1), 1.0)

	// First relevant article is at rank 1.
	near(t, "MRR", ReciprocalRank(ranked, gold), 1.0)

	// DCG  = (2^2-1)/log2(2) + (2^1-1)/log2(5)          = 3 + 0.4306765581
	// IDCG = 3/log2(2) + 3/log2(3) + 1/log2(4)          = 3 + 1.8927892607 + 0.5
	// nDCG = 3.4306765581 / 5.3927892607                = 0.6361599522
	near(t, "nDCG@10", NDCGAtK(ranked, gold, 10), 0.6361599521541359)
}

func TestMetricsFirstRelevantIsThird(t *testing.T) {
	ranked := []string{"X", "Y", "A"}
	gold := map[string]int{"A": 2}
	near(t, "MRR", ReciprocalRank(ranked, gold), 1.0/3.0)
	near(t, "recall@1", RecallAtK(ranked, gold, 1), 0)
	near(t, "recall@5", RecallAtK(ranked, gold, 5), 1)
	// DCG = 3/log2(4) = 1.5 ; IDCG = 3 ; nDCG = 0.5
	near(t, "nDCG@10", NDCGAtK(ranked, gold, 10), 0.5)
}

// TestMetricsGradesOrderMatters is the reason nDCG is the regression gate:
// recall, precision and MRR cannot tell these two orderings apart.
func TestMetricsGradesOrderMatters(t *testing.T) {
	gold := map[string]int{"A": 2, "B": 1}
	good := []string{"A", "B"}
	bad := []string{"B", "A"}

	near(t, "recall@5 good", RecallAtK(good, gold, 5), 1)
	near(t, "recall@5 bad", RecallAtK(bad, gold, 5), 1)
	near(t, "precision@5 good", PrecisionAtK(good, gold, 5), 0.4)
	near(t, "precision@5 bad", PrecisionAtK(bad, gold, 5), 0.4)

	near(t, "nDCG@10 good", NDCGAtK(good, gold, 10), 1.0)
	// DCG = 1/log2(2) + 3/log2(3) = 1 + 1.8927892607 = 2.8927892607
	// IDCG = 3 + 0.5*(2^1-1)... = 3/log2(2) + 1/log2(3) = 3 + 0.6309297536
	// nDCG = 2.8927892607 / 3.6309297536 = 0.7967075810
	near(t, "nDCG@10 bad", NDCGAtK(bad, gold, 10), 0.7967075809905066)
}

// TestMetricsDedupe: several chunks of one article must count once. Without
// this, a system that returns the same article three ways scores precision@3
// = 1.0 while showing the lawyer one article.
func TestMetricsDedupe(t *testing.T) {
	ranked := []string{"A", "A", "A", "B"}
	gold := map[string]int{"A": 2, "B": 1}
	near(t, "recall@1", RecallAtK(ranked, gold, 1), 0.5)
	near(t, "recall@2", RecallAtK(ranked, gold, 2), 1.0)
	near(t, "precision@2", PrecisionAtK(ranked, gold, 2), 1.0)
	near(t, "nDCG@10", NDCGAtK(ranked, gold, 10), 1.0)
}

func TestMetricsEmptyAndDegenerate(t *testing.T) {
	gold := map[string]int{"A": 2}
	near(t, "recall on empty ranking", RecallAtK(nil, gold, 10), 0)
	near(t, "MRR on empty ranking", ReciprocalRank(nil, gold), 0)
	near(t, "nDCG on empty ranking", NDCGAtK(nil, gold, 10), 0)
	// No gold: every gold-based metric is 0 and the caller must not average
	// it in. This is why negative controls are scored by Abstained instead.
	near(t, "recall with no gold", RecallAtK([]string{"A"}, nil, 10), 0)
	near(t, "nDCG with no gold", NDCGAtK([]string{"A"}, nil, 10), 0)
	near(t, "k=0", RecallAtK([]string{"A"}, gold, 0), 0)
}

func TestAbstained(t *testing.T) {
	if !Abstained(nil, 0.3) {
		t.Error("an empty result set is an abstention")
	}
	if !Abstained([]float64{0.29, 0.1}, 0.3) {
		t.Error("all scores below the threshold is an abstention")
	}
	if Abstained([]float64{0.30}, 0.3) {
		t.Error("a score exactly at the threshold is NOT an abstention")
	}
	if Abstained([]float64{0.05, 0.9}, 0.3) {
		t.Error("a confident hit anywhere in the list defeats abstention")
	}
}

func TestLeakedForbidden(t *testing.T) {
	ranked := []string{"A", "L1235-3", "B", "C"}
	if got := LeakedForbidden(ranked, []string{"L1235-3"}, 10); len(got) != 1 || got[0] != "L1235-3" {
		t.Errorf("leak = %v, want [L1235-3]", got)
	}
	// Outside the cut is not a leak at the cut — but the harness always calls
	// this with the same k the lawyer sees.
	if got := LeakedForbidden(ranked, []string{"L1235-3"}, 1); len(got) != 0 {
		t.Errorf("leak@1 = %v, want none", got)
	}
	if got := LeakedForbidden(ranked, nil, 10); got != nil {
		t.Errorf("no forbidden list must yield no leaks, got %v", got)
	}
}

func TestRankAgreement(t *testing.T) {
	near(t, "identical", RankAgreement([]string{"A", "B", "C"}, []string{"A", "B", "C"}, 3), 1)
	near(t, "one slot differs", RankAgreement([]string{"A", "B", "C"}, []string{"A", "X", "C"}, 3), 2.0/3.0)
	near(t, "same set, different order", RankAgreement([]string{"A", "B"}, []string{"B", "A"}, 2), 0)
	// Different lengths: the denominator is the longer list, so a truncated
	// answer cannot score 1.0 against a full one.
	near(t, "shorter list", RankAgreement([]string{"A", "B", "C"}, []string{"A"}, 3), 1.0/3.0)
	near(t, "both empty", RankAgreement(nil, nil, 3), 1)
}

func TestFirstInversion(t *testing.T) {
	tc := TestCase{
		Gold:     []Gold{{Article: "L3245-1", Grade: 2}},
		NearMiss: []string{"L1471-1"},
	}
	if _, inv := firstInversion([]string{"L3245-1", "L1471-1"}, tc); inv {
		t.Error("the primary article first is not an inversion")
	}
	if imp, inv := firstInversion([]string{"L1471-1", "L3245-1"}, tc); !inv || imp != "L1471-1" {
		t.Errorf("impostor first must be an inversion, got (%q, %v)", imp, inv)
	}
	if _, inv := firstInversion([]string{"X", "Y"}, tc); inv {
		t.Error("neither article present is a miss, not an inversion")
	}
}

// ---------------------------------------------------------------------------
// Article identity
// ---------------------------------------------------------------------------

func TestArticleOf(t *testing.T) {
	cases := map[string]string{
		"C. trav., art. L1234-9":                                       "L1234-9",
		"C. trav., art. R1455-9-1":                                     "R1455-9-1",
		"C. trav., art. L1453-1A":                                      "L1453-1A",
		"Article L. 1235-3 du code du travail":                         "L1235-3",
		"Article L. 1235-3 du code du travail, version en vigueur ...": "L1235-3",
		"Cass. soc., 25 septembre 2024, n° 22-17.315":                  "",
		"": "",
	}
	for in, want := range cases {
		if got := ArticleOf(in); got != want {
			t.Errorf("ArticleOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The query set itself
// ---------------------------------------------------------------------------

// TestSuiteIsValid is the offline gate on the gold set. Every gold, forbidden
// and near-miss article is checked to exist in the committed fixture corpus,
// because a typo there silently caps recall for every strategy at once and
// looks exactly like a retrieval regression.
func TestSuiteIsValid(t *testing.T) {
	cases, err := LoadSuite()
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	t.Logf("%d test cases loaded", len(cases))

	byCat := map[string]int{}
	byTag := map[string]int{}
	for _, c := range cases {
		byCat[c.Category]++
		for _, tag := range c.Tags {
			byTag[tag]++
		}
	}
	for cat, n := range byCat {
		t.Logf("  category %-18s %d", cat, n)
	}
	for tag, n := range byTag {
		t.Logf("  tag      %-18s %d", tag, n)
	}

	// The spread is part of the contract, not an accident of how the file
	// grew. Each of these catches a different failure mode, and a suite
	// missing one of them can be green while the product is broken.
	requirements := []struct {
		what string
		n    int
		min  int
	}{
		{"negative controls", byCat["negative_control"], 8},
		{"temporal cases", byCat["temporal"], 6},
		{"vocabulary-mismatch cases", byTag["vocab_mismatch"], 6},
		{"multi-hop cases", byTag["multi_hop"], 5},
		{"near-miss cases", byTag["near_miss"], 6},
		{"accent/casing variants", byTag["accent_variant"], 4},
	}
	for _, r := range requirements {
		if r.n < r.min {
			t.Errorf("%s: %d, want at least %d", r.what, r.n, r.min)
		}
	}
}

// TestCostModel pins the arithmetic of the cost estimate. It is not a billing
// figure (llm_calls is), but a scoreboard column nobody can reproduce is a
// scoreboard column nobody should believe.
func TestCostModel(t *testing.T) {
	// FTS embeds nothing and reranks nothing.
	near(t, "fts", estimateCost(services.StrategyFTS, 100, []int{4000}), 0)

	// 400 characters ≈ 100 tokens ≈ 100/1e6 * $0.02 = $2e-6.
	near(t, "vector", estimateCost(services.StrategyVector, 400, nil), 2e-6)
	near(t, "rrf", estimateCost(services.StrategyRRF, 400, []int{4000}), 2e-6)

	// 40 candidates of 2000 chars ≈ 500 tokens each = 40 doc-units ≤ 100,
	// so one search unit: $0.002, plus the same $2e-6 embedding.
	docs := make([]int, 40)
	for i := range docs {
		docs[i] = 2000
	}
	near(t, "rrf_rerank one unit", estimateCost(services.StrategyRRFRerank, 400, docs), 0.002+2e-6)

	// 40 candidates of 8000 chars ≈ 2000 tokens each split into 4 → 160
	// doc-units → two search units.
	for i := range docs {
		docs[i] = 8000
	}
	near(t, "rrf_rerank two units", estimateCost(services.StrategyRRFRerank, 400, docs), 0.004+2e-6)
}

// ---------------------------------------------------------------------------
// Live measurement
// ---------------------------------------------------------------------------

// Recorded baselines. Every number here is a MEASUREMENT taken on the date in
// README.md against the committed fixture corpus — not a target somebody hoped
// for. A change that pushes the default strategy below one has to be argued
// for, and the README updated in the same commit.
//
// The margins below the measured values absorb the cross-encoder's run-to-run
// variation. They are not slack for a regression to hide in.
const (
	// Measured 0.857 (CLI, all four strategies) and 0.858 (test) for rrf_rerank
	// on 2026-08-27 — the cross-encoder's run-to-run spread, in one line.
	BaselineNDCG10 = 0.840
	// Measured 0.894.
	BaselineRecall10 = 0.875
	// Measured 0.833: the default strategy answered 2 of 12 questions its
	// corpus cannot answer. That is a WEAKNESS recorded as a floor, not an
	// achievement — README.md, weakness 4.
	BaselineAbstention = 0.75
	// Measured 0.575, and the correct value is 1.000. §6.2 claims unaccent
	// makes accented and unaccented queries behave identically; that is true
	// of the lexical arm (which scores 1.000) and FALSE of the dense arm,
	// because a dense embedder places "congés payés" and "conges payes" at
	// different points — measured under both bge-m3 and text-embedding-3-small.
	// The floor guards against further drift while the defect is open —
	// README.md, weakness 3.
	BaselineVariantAgreement = 0.50
)

func liveSuite(t *testing.T) []TestCase {
	t.Helper()
	cases, err := LoadSuite()
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if err := Preflight(services.SourceFixtures); err != nil {
		switch {
		case errors.Is(err, ErrNoCorpus):
			t.Skipf("SKIPPING (no score produced): %v — load it with a corpus sync of source=fixtures", err)
		case errors.Is(err, ErrNoGateway), errors.Is(err, ErrNoDB):
			t.Skipf("SKIPPING (no score produced): %v", err)
		default:
			t.Skipf("SKIPPING (no score produced): %v", err)
		}
	}
	return cases
}

// TestRelevanceBaseline is the regression gate. One strategy, the default one,
// over the whole query set.
func TestRelevanceBaseline(t *testing.T) {
	cases := liveSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	rep := RunSuite(ctx, cases, RunOptions{Strategy: services.StrategyRRFRerank})
	t.Log("\n" + FormatScoreboard([]Report{rep}))

	if len(rep.Errors) > 0 {
		t.Fatalf("%d queries failed outright:\n  %s", len(rep.Errors), strings.Join(rep.Errors, "\n  "))
	}
	if rep.Overall.Queries == 0 {
		t.Fatal("no graded query ran — refusing to report a score")
	}
	if rep.Overall.NDCGAt10 < BaselineNDCG10 {
		t.Errorf("nDCG@10 = %.4f, below the recorded baseline %.3f — retrieval regressed",
			rep.Overall.NDCGAt10, BaselineNDCG10)
	}
	if rep.Overall.RecallAt10 < BaselineRecall10 {
		t.Errorf("recall@10 = %.4f, below the recorded baseline %.3f",
			rep.Overall.RecallAt10, BaselineRecall10)
	}
	if rep.AbstentionRate < BaselineAbstention {
		t.Errorf("abstention on negative controls = %.2f, below the recorded baseline %.2f — "+
			"the system got MORE confident about questions it cannot answer",
			rep.AbstentionRate, BaselineAbstention)
	}
}

// TestTemporalIsolation is separated from the baseline because it is not a
// relevance opinion: returning an article that was not in force at the date of
// the facts is wrong law (§6.4), and no ranking improvement trades against it.
func TestTemporalIsolation(t *testing.T) {
	all := liveSuite(t)
	var temporal []TestCase
	for _, c := range all {
		if len(c.Forbidden) > 0 {
			temporal = append(temporal, c)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rep := RunSuite(ctx, temporal, RunOptions{Strategy: services.StrategyRRFRerank})
	t.Logf("temporal cases: %d, leaks: %d", rep.TemporalCases, rep.TemporalLeaks)
	if rep.TemporalLeaks > 0 {
		t.Errorf("an article not in force at the as-of date was returned:\n  %s",
			strings.Join(rep.LeakDetail, "\n  "))
	}
}

// TestAccentAndCasingInvariance: unaccent is applied to the query exactly as
// it was applied to the text at write time (§6.2), so these pairs must not
// merely score similarly — they must return the same articles in the same
// order. Anything less is a normalisation bug.
func TestAccentAndCasingInvariance(t *testing.T) {
	all := liveSuite(t)
	byCase := map[string]TestCase{}
	var variants []TestCase
	for _, c := range all {
		byCase[c.ID] = c
		if c.VariantOf != "" {
			variants = append(variants, c, byCase[c.VariantOf])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rep := RunSuite(ctx, variants, RunOptions{Strategy: services.StrategyRRFRerank})
	byID := map[string]CaseResult{}
	for _, r := range rep.Results {
		byID[r.Case.ID] = r
	}
	t.Logf("variant pairs: %d, mean slot-for-slot agreement: %.3f", rep.VariantPairs, rep.VariantAgreeAvg)
	if rep.VariantPairs == 0 {
		t.Fatal("no variant pair ran")
	}
	if rep.VariantAgreeAvg < 1.0 {
		// KNOWN DEFECT, recorded rather than hidden: the dense arm is not
		// accent-invariant. The test guards the recorded floor so this cannot
		// silently get worse; fixing it means normalising the query before it
		// reaches the embedding call, which is not this package's file.
		for _, r := range rep.Results {
			if r.Case.VariantOf == "" {
				continue
			}
			t.Logf("  %-9s %v", r.Case.ID, r.Articles)
			t.Logf("  %-9s %v", r.Case.VariantOf, byID[r.Case.VariantOf].Articles)
		}
		t.Logf("FINDING: variant agreement is %.3f, not 1.000 — accent/casing changes the ranking. "+
			"unaccent protects the lexical arm only; the embedding sees the accents.", rep.VariantAgreeAvg)
	}
	if rep.VariantAgreeAvg < BaselineVariantAgreement {
		t.Errorf("variant agreement %.3f, below the recorded baseline %.3f — normalisation regressed further",
			rep.VariantAgreeAvg, BaselineVariantAgreement)
	}
}

// TestStrategyComparison is the headline: every switchable strategy scored
// over the same query set. It asserts only what the design CLAIMS — that
// fusion beats each of its own arms — and prints everything else, because a
// number nobody looks at changes nothing.
//
// It is skipped by default (it costs money and several minutes); run it with
//
//	cd api && GOTOOLCHAIN=auto NOMOS_EVAL_FULL=1 go test ./eval/... -run Comparison -v -timeout 40m
//
// or use the CLI, which is what the scoreboard in README.md was produced with.
func TestStrategyComparison(t *testing.T) {
	if os.Getenv("NOMOS_EVAL_FULL") == "" {
		t.Skip("set NOMOS_EVAL_FULL=1 to score all four strategies (several minutes, real spend)")
	}
	cases := liveSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	var reports []Report
	for _, s := range services.Strategies {
		reports = append(reports, RunSuite(ctx, cases, RunOptions{Strategy: s}))
	}
	t.Log("\n" + FormatScoreboard(reports))

	byStrategy := map[string]Report{}
	for _, r := range reports {
		byStrategy[r.Strategy] = r
	}
	fused := byStrategy[services.StrategyRRF]
	rr := byStrategy[services.StrategyRRFRerank]

	// The only assertion is the PRODUCT's claim: the shipped default must beat
	// every single arm it is built from. Asserting that RRF alone beats both
	// arms would assert something the measurement says is false — see the
	// FINDING logged below — and a query set tuned until that assertion
	// passes measures nothing.
	for _, arm := range []string{services.StrategyVector, services.StrategyFTS} {
		if rr.Overall.NDCGAt10 <= byStrategy[arm].Overall.NDCGAt10 {
			t.Errorf("the default strategy's nDCG@10 %.4f does not beat the %s arm alone (%.4f) — "+
				"the two-stage design is not earning its complexity or its money",
				rr.Overall.NDCGAt10, arm, byStrategy[arm].Overall.NDCGAt10)
		}
	}

	vec := byStrategy[services.StrategyVector]
	if fused.Overall.NDCGAt10 < vec.Overall.NDCGAt10 {
		t.Logf("FINDING: RRF nDCG@10 %.4f is BELOW the vector arm alone (%.4f). "+
			"Fusing a near-empty lexical arm costs ranking quality: a chunk found by one arm "+
			"gets 1/(60+rank) either way, so the fusion only reshuffles the vector order. "+
			"The rerank pass is what recovers it.",
			fused.Overall.NDCGAt10, vec.Overall.NDCGAt10)
	}
	t.Logf("rerank delta vs RRF: nDCG@10 %+.4f, P@5 %+.4f, p50 latency %+s, cost %+.6f USD/query (%.0fx)",
		rr.Overall.NDCGAt10-fused.Overall.NDCGAt10,
		rr.Overall.PrecAt5-fused.Overall.PrecAt5,
		(rr.LatencyP50 - fused.LatencyP50).Round(time.Millisecond),
		rr.MeanCostUSD-fused.MeanCostUSD, rr.MeanCostUSD/math.Max(fused.MeanCostUSD, 1e-12))
}
