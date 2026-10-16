// Command cmd runs the Nomos agent's production-model evaluation and its
// release gate. Run it from api/. It reads
// no .env file: export DATABASE_URL (the evaluation's own database, never the
// developer's), the gateway key, the PISTE credentials and the models first.
//
//	go run ./eval/agent/cmd -preflight -config ../artifacts/release-eval-config.json -require-models -require-dependencies
//	go run ./eval/agent/cmd -suite fr_v1 -agent-suite agent_failures_v1 -tier fast \
//	    -config ../artifacts/release-eval-config.json -require-models -require-dependencies \
//	    -budget-usd 25 -timeout 3h -report ../artifacts/agent-eval-fast.json
//	go run ./eval/agent/cmd -suite fr_v1 -tier smoke -case <id> -config … -budget-usd 5 -report …
//	go run ./eval/agent/cmd -verify-report ../artifacts/agent-eval-full.json \
//	    -review ../artifacts/agent-review.json -config ../artifacts/release-eval-config.json -gate
//
// Exit status: 0 passed / allowed, 1 failed / refused, 2 usage error. See
// api/eval/agent/README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agent "github.com/Arkel-ai/nomos/api/eval/agent"
	u "github.com/Arkel-ai/nomos/api/utils"
)

func main() {
	var (
		preflight    = flag.Bool("preflight", false, "check the release configuration and every dependency without spending")
		configPath   = flag.String("config", "", "release evaluation config (JSON): models, tariffs, approved domains")
		requireModel = flag.Bool("require-models", false, "fail unless the environment's models and settings are exactly the release config's")
		requireDeps  = flag.Bool("require-dependencies", false, "fail on any missing dependency or changed fixture instead of reporting it")
		suite        = flag.String("suite", "", "French suite (fr_v1)")
		agentSuite   = flag.String("agent-suite", "", "agent-specific suite (agent_failures_v1)")
		tier         = flag.String("tier", "", "smoke | fast | full")
		caseID       = flag.String("case", "", "smoke tier only: the one case to run")
		budget       = flag.Float64("budget-usd", 0, "stopping limit in USD: reaching it ends the run as incomplete_budget")
		timeout      = flag.Duration("timeout", 3*time.Hour, "wall-clock limit for the whole run")
		reportPath   = flag.String("report", "", "where to write the report (outputs go beside it, in <report>.outputs/)")
		verifyPath   = flag.String("verify-report", "", "verify a report's integrity (and a review with -review)")
		reviewPath   = flag.String("review", "", "the lawyer's output-hash-bound review file")
		gate         = flag.Bool("gate", false, "with -verify-report: allow release only for a passing, reviewed full-tier report of this commit and config")
	)
	flag.Parse()
	u.InitLogger(firstNonEmpty(os.Getenv("LOG_LEVEL"), "INFO"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manifest, err := agent.LoadManifest()
	if err != nil {
		fail(2, "%v", err)
	}
	var (
		release   agent.ReleaseConfig
		configSHA string
	)
	if *configPath != "" {
		release, configSHA, err = agent.LoadReleaseConfig(*configPath)
		if err != nil {
			fail(2, "%v", err)
		}
	}
	opts := agent.Options{
		Release: release, ConfigSHA: configSHA,
		RequireModels: *requireModel, RequireDependencies: *requireDeps,
		Tier: *tier, CaseID: *caseID, BudgetUSD: *budget, Timeout: *timeout, ReportPath: *reportPath,
		FixtureRoot: firstNonEmpty(os.Getenv("NOMOS_EVAL_TESTDATA"), "../testdata"),
	}

	switch {
	case *preflight:
		if *configPath == "" {
			fail(2, "-preflight needs -config")
		}
		models, deps, notes := agent.Preflight(ctx, opts, manifest)
		for _, n := range notes {
			fmt.Println("  note: " + n)
		}
		for _, p := range models {
			fmt.Println("  model/config: " + p)
		}
		for _, p := range deps {
			fmt.Println("  dependency: " + p)
		}
		if (*requireModel && len(models) > 0) || (*requireDeps && len(deps) > 0) {
			fail(1, "preflight refused (%d model/config problem(s), %d dependency problem(s))", len(models), len(deps))
		}
		fmt.Printf("preflight ok (%d model/config warning(s), %d dependency warning(s))\n", len(models), len(deps))

	case *verifyPath != "":
		rep, err := agent.ReadReport(*verifyPath)
		if err != nil {
			fail(2, "%v", err)
		}
		fmt.Print(rep.Summary())
		var review *agent.Review
		if *reviewPath != "" {
			rv, err := agent.ReadReview(*reviewPath)
			if err != nil {
				fail(2, "%v", err)
			}
			review = &rv
		}
		var problems []string
		if *gate {
			if *configPath == "" {
				fail(2, "-gate needs -config: the configuration fingerprint is recomputed from it")
			}
			suites := loadSuites(manifest, agent.SuiteFR, agent.SuiteAgentFailures)
			current := agent.StaticFingerprint(configSHA, suites, manifest)
			problems = agent.Gate(rep, review, current)
		} else {
			problems = agent.VerifyReport(rep)
			if review != nil {
				problems = append(problems, agent.CheckReview(rep, *review)...)
			}
		}
		if len(problems) > 0 {
			fail(1, "refused:\n  - %s", strings.Join(problems, "\n  - "))
		}
		if *gate {
			fmt.Println("GATE: release allowed for commit " + rep.Fingerprint.Commit)
		} else {
			fmt.Println("report verified")
		}

	default:
		if *tier == "" {
			fail(2, "nothing to do: -preflight, -verify-report or -tier")
		}
		if *configPath == "" {
			fail(2, "a run needs -config: the release configuration it is evaluated under")
		}
		var ids []string
		for _, s := range []string{*suite, *agentSuite} {
			if s != "" {
				ids = append(ids, s)
			}
		}
		if len(ids) == 0 {
			fail(2, "a run needs -suite and/or -agent-suite")
		}
		suites := loadSuites(manifest, ids...)
		rep, err := agent.Execute(ctx, opts, suites, manifest)
		if err != nil {
			fail(1, "%v", err)
		}
		fmt.Print(rep.Summary())
		if *reportPath != "" {
			fmt.Printf("report: %s\noutputs: %s\n", *reportPath, agent.OutputsDir(*reportPath))
		}
		if rep.Status != agent.StatusPassed {
			os.Exit(1)
		}
	}
}

func loadSuites(m agent.Manifest, ids ...string) []agent.Suite {
	var out []agent.Suite
	for _, id := range ids {
		s, err := agent.LoadSuite(id, m)
		if err != nil {
			fail(2, "%v", err)
		}
		out = append(out, s)
	}
	return out
}

func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nomos-agent-eval: "+format+"\n", args...)
	os.Exit(code)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
