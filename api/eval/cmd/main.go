// Command eval prints the human-readable relevance scoreboard.
//
//	cd api && GOTOOLCHAIN=auto go run ./eval/cmd -strategy all
//
// It scores every switchable retrieval strategy over the same query set and
// prints them side by side, with latency and modelled cost, so the question
// "does the reranker earn its money" has an answer rather than an opinion
// (docs/PLAN.md §6.2, §15 open decision 3).
//
// It exits non-zero and says WHY when the corpus is not loaded or the gateway
// has no key. It never prints a score it did not measure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Arkel-ai/nomos/api/eval"
	"github.com/Arkel-ai/nomos/api/services"
)

func main() {
	strategy := flag.String("strategy", "all", "vector | fts | rrf | rrf_rerank | all")
	source := flag.String("source", services.SourceFixtures, "legal_documents.source to score against")
	topK := flag.Int("top-k", 10, "how many hits the metrics are computed over")
	category := flag.String("category", "", "only run cases in this category")
	caseID := flag.String("case", "", "only run this case id (implies -strategy all)")
	worst := flag.Int("worst", 10, "how many worst-scoring queries to list for the default strategy (0 to skip)")
	jsonOut := flag.String("json", "", "also write the raw per-case results here, so a scoreboard can be re-rendered without paying to re-run")
	timeout := flag.Duration("timeout", 30*time.Minute, "wall-clock budget for the whole run")
	flag.Parse()

	cases, err := eval.LoadSuite()
	if err != nil {
		fail("%v", err)
	}
	if *category != "" {
		cases = filter(cases, func(c eval.TestCase) bool { return c.Category == *category })
	}
	if *caseID != "" {
		cases = filter(cases, func(c eval.TestCase) bool { return c.ID == *caseID })
	}
	if len(cases) == 0 {
		fail("no test cases matched the filters")
	}

	if err := eval.Preflight(*source); err != nil {
		// A missing corpus or a missing key is a "cannot measure", not a
		// failure of retrieval. Say which, and say how to fix it.
		fmt.Fprintf(os.Stderr, "\ncannot run the evaluation: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "  corpus:  POST /api/corpus/sync with {\"source\":\"fixtures\"} (admin), or run\n")
		fmt.Fprintf(os.Stderr, "           the corpus sync from the app, then re-run this command.\n")
		fmt.Fprintf(os.Stderr, "  key:     set OPENROUTER_API_KEY in api/.env.\n\n")
		os.Exit(2)
	}

	strategies := services.Strategies
	if *strategy != "all" {
		s, err := services.ParseStrategy(*strategy)
		if err != nil {
			fail("%v", err)
		}
		strategies = []string{s}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	reports := make([]eval.Report, 0, len(strategies))
	for _, s := range strategies {
		fmt.Fprintf(os.Stderr, "scoring %-11s over %d cases ... ", s, len(cases))
		start := time.Now()
		rep := eval.RunSuite(ctx, cases, eval.RunOptions{Strategy: s, Source: *source, TopK: *topK})
		fmt.Fprintf(os.Stderr, "done in %s\n", time.Since(start).Round(time.Second))
		reports = append(reports, rep)
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "\ninterrupted — the scoreboard below is partial\n")
			break
		}
	}

	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			fail("writing %s: %v", *jsonOut, err)
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fail("writing %s: %v", *jsonOut, err)
		}
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "raw results written to %s\n", *jsonOut)
	}

	fmt.Print(eval.FormatScoreboard(reports))

	if *worst > 0 {
		for _, r := range reports {
			if r.Strategy == services.StrategyRRFRerank {
				fmt.Print(eval.FormatWorstCases(r, *worst))
			}
		}
	}
}

func filter(cases []eval.TestCase, keep func(eval.TestCase) bool) []eval.TestCase {
	out := cases[:0:0]
	for _, c := range cases {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, strings.TrimRight(format, "\n")+"\n", args...)
	os.Exit(1)
}
