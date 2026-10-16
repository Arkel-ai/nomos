package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Arkel-ai/nomos/api/config"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// RequiredBinaries are the external tools ingestion, OCR and rendering need.
var RequiredBinaries = []string{"pandoc", "pdftotext", "pdftoppm", "tesseract"}

// Execute runs a tier and returns its sealed report (also written to
// opts.ReportPath after every trial, so an interrupted run leaves an honest
// partial report). A configuration or dependency problem detected before
// anything is spent is an error, not a report.
func Execute(ctx context.Context, opts Options, suites []Suite, m Manifest) (*Report, error) {
	opts = opts.withDefaults()
	var fr, ag *Suite
	for i := range suites {
		switch suites[i].Suite {
		case SuiteFR:
			fr = &suites[i]
		case SuiteAgentFailures:
			ag = &suites[i]
		}
	}
	trials, err := PlanTrials(opts.Tier, fr, ag, opts.CaseID)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		Version: ReportVersion, GeneratedAt: time.Now().UTC(), Tier: opts.Tier, Case: opts.CaseID,
		ReleaseName: opts.Release.Name, BudgetUSD: opts.BudgetUSD,
		Flags: RunFlags{RequireModels: opts.RequireModels, RequireDependencies: opts.RequireDependencies,
			NoDotenv: true, Timeout: opts.Timeout.String()},
	}
	for _, s := range suites {
		rep.Suites = append(rep.Suites, s.Suite)
	}
	if w, ok := Workloads[opts.Tier]; ok {
		rep.Workload = w
	}
	rep.Fingerprint = StaticFingerprint(opts.ConfigSHA, suites, m)

	// Everything that costs nothing comes first: a run that cannot be valid
	// must not spend a cent finding out. No .env file is read (Boot).
	rep.ProductionProblems = CheckProduction(config.Load(), opts.Release)
	if opts.RequireModels && len(rep.ProductionProblems) > 0 {
		return nil, fmt.Errorf("-require-models: the environment is not the release configuration:\n  - %s", strings.Join(rep.ProductionProblems, "\n  - "))
	}
	for _, p := range VerifyFixtures(m, opts.FixtureRoot, dossiersOf(trials, m)) {
		rep.DependencyProblems = append(rep.DependencyProblems, fmt.Sprintf("fixture %s: %s", p.Path, p.Reason))
	}
	for _, bin := range RequiredBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			rep.DependencyProblems = append(rep.DependencyProblems, bin+" is not on PATH")
		}
	}
	if opts.RequireDependencies && len(rep.DependencyProblems) > 0 {
		return nil, fmt.Errorf("-require-dependencies:\n  - %s", strings.Join(rep.DependencyProblems, "\n  - "))
	}

	env, err := Boot(ctx, opts, m)
	if err != nil {
		return nil, err
	}
	defer env.Close()
	// Never a warning: a foreign runner on the same database would claim
	// this run's steps and execute them with ITS configuration, outside the
	// proxy and the model check, and its writes would land here.
	if n, err := ForeignClients(); err != nil {
		return nil, fmt.Errorf("could not check the evaluation database for other clients: %w", err)
	} else if n > 0 {
		return nil, fmt.Errorf("%d other client(s) are connected to the evaluation database: a foreign runner would execute this run's steps", n)
	}
	if NeedsLaw(trials) {
		n, err := env.SeedCorpus(ctx)
		if err != nil || n == 0 {
			problem := "the public legal corpus is not loaded (corpus autoseed found nothing to load)"
			if opts.RequireDependencies {
				return nil, errors.New("-require-dependencies: " + problem)
			}
			rep.DependencyProblems = append(rep.DependencyProblems, problem)
		}
	}
	rep.Effective = EffectiveConfig(env.Config(), env.Upstream())
	if sha, err := CorpusSHA256(); err == nil {
		rep.CorpusSHA = sha
	}
	rep.SkillSHA = env.SkillSHA256()

	budget := NewBudget(u.DB, env.Firm().ID, opts.BudgetUSD, env.started)
	log.Infof("Evaluation started (tier=%s trials=%d budget=%.2f timeout=%s)", opts.Tier, len(trials), opts.BudgetUSD, opts.Timeout)
	env.RunAll(ctx, trials, budget, env.started.Add(opts.Timeout), rep)
	if opts.ReportPath != "" {
		if err := WriteReport(opts.ReportPath, rep); err != nil {
			return rep, err
		}
	} else {
		rep.Seal()
	}
	log.Infof("Evaluation finished (status=%s passed=%d/%d cost=%.4f unknown=%d)",
		rep.Status, rep.Counts.Passed, rep.Counts.Planned, rep.Usage.CostUSD, rep.Usage.UnknownUsage)
	return rep, nil
}

// RunAll runs the trials in order into rep, stopping — never skipping — on
// budget, deadline or interruption: whatever did not run stays in the report
// as not_run, so the denominator is always the planned workload. A partial
// report is written after every trial when rep is bound to a path.
func (e *Env) RunAll(ctx context.Context, trials []Trial, budget *Budget, deadline time.Time, rep *Report) {
	ran := 0
	for _, t := range trials {
		if ctx.Err() != nil {
			rep.StoppedBy = "interrupted"
			break
		}
		if done, spent, err := budget.Exhausted(); err == nil && done {
			log.Warnf("Budget exhausted before a trial (spent=%.4f limit=%.2f)", spent, budget.LimitUSD)
			rep.StoppedBy = StatusIncompleteBudget
			break
		}
		if time.Now().After(deadline) {
			rep.StoppedBy = StatusIncompleteTimeout
			break
		}
		res := e.RunTrial(ctx, t, budget, deadline)
		rep.Trials = append(rep.Trials, res)
		ran++
		if e.opts.ReportPath != "" {
			partial := *rep
			partial.Trials = append(append([]TrialResult(nil), rep.Trials...), notRun(trials[ran:])...)
			partial.Usage, _ = budget.Usage()
			partial.Aggregate()
			_ = WriteReport(e.opts.ReportPath, &partial)
		}
		if res.Status == StatusIncompleteBudget || res.Status == StatusIncompleteTimeout {
			rep.StoppedBy = res.Status
			break
		}
	}
	rep.Trials = append(rep.Trials, notRun(trials[ran:])...)
	if us, err := budget.Usage(); err == nil {
		rep.Usage = us
	}
	if us, err := CollectUsage(u.DB, "l.tenant_id = ? AND l.run_id IN ?", e.firm.ID, e.IngestionRuns()); err == nil {
		rep.IngestionUsage = us
	}
	if us, err := CollectUsage(u.DB, "l.tenant_id = '' AND l.started_at >= ?", e.started.Unix()); err == nil {
		rep.IngestionUsage.Add(us)
	}
	outside := e.proxy.TakeOutside()
	rep.IngestionModelIssues = ModelIssues(e.opts.Release, outside)
	rep.IngestionReturnedModels = ReturnedModels(outside)
	rep.Aggregate()
}

func notRun(trials []Trial) []TrialResult {
	out := make([]TrialResult, 0, len(trials))
	for _, t := range trials {
		out = append(out, TrialResult{ID: t.ID, Suite: t.Suite, CaseID: t.Case.ID, Class: t.Case.Class,
			State: t.Case.State, Kind: t.Case.Kind, N: t.N, Status: StatusNotRun})
	}
	return out
}

// dossiersOf is every fixture dossier the trials need, plus the states'.
func dossiersOf(trials []Trial, m Manifest) []string {
	var out []string
	for _, t := range trials {
		if t.Case.Dossier != "" && !slices.Contains(out, t.Case.Dossier) {
			out = append(out, t.Case.Dossier)
		}
	}
	return out
}

// Preflight checks everything a paid run depends on, without spending: the
// environment against the release config, the gateway's model list and
// prices against the dated tariff, the fixtures against the manifest, the
// binaries, the database (extensions, isolation), the blob store and the
// connectors. It returns the model problems and the dependency problems
// separately: -require-models and -require-dependencies make them fatal.
func Preflight(ctx context.Context, opts Options, m Manifest) (models, deps []string, notes []string) {
	cfg := config.Load() // the exported environment alone: no .env file is read
	models = append(models, CheckProduction(cfg, opts.Release)...)
	if cfg.OpenRouterAPIKey == "" {
		deps = append(deps, "OPENROUTER_API_KEY is not set")
	} else {
		listed, problems, err := CheckTariffs(ctx, firstNonEmpty(opts.Upstream, cfg.OpenRouterBaseURL), cfg.OpenRouterAPIKey, opts.Release)
		if err != nil {
			deps = append(deps, "gateway: "+err.Error())
		} else {
			models = append(models, problems...)
			for _, id := range opts.Release.Models.all() {
				if t, ok := listed[id]; ok {
					notes = append(notes, fmt.Sprintf("tariff %s: $%.4f prompt / $%.4f completion per Mtok", id, t.PromptUSDPerMTok, t.CompletionUSDPerMTok))
				}
			}
		}
	}
	var all []string
	for k := range m.Dossiers {
		all = append(all, k)
	}
	for _, p := range VerifyFixtures(m, opts.FixtureRoot, all) {
		deps = append(deps, fmt.Sprintf("fixture %s: %s", p.Path, p.Reason))
	}
	for _, bin := range RequiredBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			deps = append(deps, bin+" is not on PATH")
		}
	}
	if cfg.PisteClientID == "" || cfg.PisteClientSecret == "" {
		deps = append(deps, "PISTE credentials are not set: Légifrance and Judilibre cannot be reached")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		deps = append(deps, "DATABASE_URL is not exported (the evaluation never reads a .env file nor uses the developer database)")
		return models, deps, notes
	}
	if databaseName(dsn) == devDatabaseName {
		deps = append(deps, fmt.Sprintf("DATABASE_URL names the %q database, the developer's: use the evaluation's own (.eval/docker-compose.yml)", devDatabaseName))
		return models, deps, notes
	}
	os.Setenv("DATABASE_URL", withAppName(dsn))
	if u.DB == nil {
		u.InitClients(config.Load())
	}
	if u.DB == nil {
		deps = append(deps, "Postgres is not reachable at DATABASE_URL")
		return models, deps, notes
	}
	var exts []string
	u.DB.Raw("SELECT extname FROM pg_extension").Scan(&exts)
	for _, want := range []string{"vector", "unaccent"} {
		if !slices.Contains(exts, want) {
			deps = append(deps, "the database lacks the "+want+" extension")
		}
	}
	if n, err := ForeignClients(); err != nil {
		deps = append(deps, "could not check the database for other clients: "+err.Error())
	} else if n > 0 {
		deps = append(deps, fmt.Sprintf("%d other client(s) are connected to the evaluation database", n))
	}
	var corpus int64
	u.DB.Table("legal_documents").Where("source <> ? AND source NOT LIKE ?", "fixtures", "test_%").Count(&corpus)
	notes = append(notes, fmt.Sprintf("public legal corpus: %d document(s) loaded (autoseed fills it before a run that needs it)", corpus))

	key := u.BlobPrefix() + "eval-preflight/probe.txt"
	probe := []byte("nomos agent eval preflight " + time.Now().UTC().Format(time.RFC3339))
	if err := u.Blob.Put(ctx, key, bytes.NewReader(probe), int64(len(probe)), "text/plain"); err != nil {
		deps = append(deps, "the blob store refused a write: "+err.Error())
	} else {
		rc, err := u.Blob.Get(ctx, key)
		if err != nil {
			deps = append(deps, "the blob store lost a write: "+err.Error())
		} else {
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, probe) {
				deps = append(deps, "the blob store returned different bytes")
			}
		}
		_ = u.Blob.Delete(ctx, key)
		notes = append(notes, "blob store: "+u.Blob.Backend())
	}
	return models, deps, notes
}

// hasher hashes ordered tuples.
type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (x *hasher) add(parts ...string) {
	for _, p := range parts {
		x.h.Write([]byte(p))
		x.h.Write([]byte{0})
	}
	x.h.Write([]byte{'\n'})
}

func (x *hasher) sum() string { return hex.EncodeToString(x.h.Sum(nil)) }
