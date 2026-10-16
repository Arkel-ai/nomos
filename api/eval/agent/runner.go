package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	nomoseval "github.com/Arkel-ai/nomos/api/eval"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The runner executes the COMPLETE agent outcome in-process, the way
// api/agent_vasseur_test.go does: the real HTTP surface (testsupport's
// router, real middleware after authentication), the real runner pools,
// Postgres, the tools, the verifier, the publisher, pandoc and the blob
// store — with every model call going through the recording Proxy to the
// configured gateway. Trials run one after another: latency is the
// product's, and every call in a trial's window is that trial's.

// Options configures one execution.
type Options struct {
	Release             ReleaseConfig
	ConfigSHA           string
	RequireModels       bool
	RequireDependencies bool
	Tier                string
	CaseID              string
	BudgetUSD           float64
	Timeout             time.Duration
	ReportPath          string
	// FixtureRoot is the repository's testdata directory.
	FixtureRoot string

	// Harness knobs. Zero values are the production-faithful defaults.
	PollEvery      time.Duration // run polling; default 2s
	TrialDeadline  time.Duration // one trial's wall clock; default 45m
	IngestDeadline time.Duration // one dossier's ingestion; default 30m
	FastRunner     bool          // compress the runner's clocks (tests only)
	Upstream       string        // gateway the proxy forwards to; default OPENROUTER_BASE_URL
}

func (o Options) withDefaults() Options {
	if o.PollEvery <= 0 {
		o.PollEvery = 2 * time.Second
	}
	if o.TrialDeadline <= 0 {
		o.TrialDeadline = 45 * time.Minute
	}
	if o.IngestDeadline <= 0 {
		o.IngestDeadline = 30 * time.Minute
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Hour
	}
	return o
}

// Env is one booted evaluation environment: one firm, one router, one proxy,
// the runner pools.
type Env struct {
	opts     Options
	cfg      config.Config
	manifest Manifest
	proxy    *Proxy
	router   *gin.Engine
	firm     testsupport.Firm
	upstream string

	poolsMu     sync.Mutex
	stopPools   context.CancelFunc
	restoreGW   func()
	prevBaseURL string
	tmpDir      string

	dossiers    map[string]*builtDossier
	competences map[string]bool
	ingestRuns  []string
	started     time.Time
}

type builtDossier struct {
	ID     string
	Key    string
	Files  map[string]string // fixture file → piece id
	ByID   map[string]string // piece id → fixture file
	Failed []string          // files whose ingestion failed
}

// appName is the application_name the evaluation's own Postgres
// connections carry, so a foreign client on the same database is visible.
const appName = "nomos-agent-eval"

// Boot prepares the environment. Nothing is spent here: no model is called.
func Boot(ctx context.Context, opts Options, m Manifest) (*Env, error) {
	opts = opts.withDefaults()
	e := &Env{opts: opts, manifest: m, dossiers: map[string]*builtDossier{}, competences: map[string]bool{}, started: time.Now()}

	// No .env file is ever read: the exported environment is the run's only
	// configuration, so api/.env's developer database can never be picked up.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not exported: the evaluation needs its own disposable database (see .eval/docker-compose.yml) and never reads a .env file")
	}
	if databaseName(dsn) == devDatabaseName {
		return nil, fmt.Errorf("DATABASE_URL names the %q database, the developer's: the evaluation runs only on its own disposable database (see .eval/docker-compose.yml)", devDatabaseName)
	}
	os.Setenv("DATABASE_URL", withAppName(dsn))

	cfg := config.Load()
	e.upstream = firstNonEmpty(opts.Upstream, cfg.OpenRouterBaseURL)
	proxy, err := StartProxy(e.upstream, cfg.OpenRouterAPIKey)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	e.proxy = proxy
	// Every model call — the gateway, which builds itself lazily, and the
	// web adapter, which reads the base URL per call — now goes through it.
	e.prevBaseURL = os.Getenv("OPENROUTER_BASE_URL")
	os.Setenv("OPENROUTER_BASE_URL", proxy.URL())
	e.cfg = config.Load()

	gin.SetMode(gin.TestMode)
	if u.DB == nil {
		u.InitClients(e.cfg)
	}
	if u.DB == nil || u.Enforcer == nil {
		e.Close()
		return nil, errors.New("Postgres/Casbin not reachable at DATABASE_URL")
	}
	e.restoreGW = u.UseGateway(proxy.URL(), e.cfg.OpenRouterAPIKey)
	if !u.LLMConfigured() {
		e.Close()
		return nil, errors.New("the LLM gateway is not configured (OPENROUTER_API_KEY)")
	}
	if u.S3 == nil {
		// The local blob backend, in a disposable directory: the eval never
		// writes into the API's own tmp/.
		dir, err := os.MkdirTemp("", "nomos-agent-eval-")
		if err != nil {
			e.Close()
			return nil, err
		}
		e.tmpDir = dir
		u.SetTmpRoot(dir)
	}

	if opts.FastRunner {
		runner.PollInterval = 25 * time.Millisecond
		runner.BaseBackoff = 20 * time.Millisecond
		runner.MaxBackoff = 200 * time.Millisecond
		runner.DefaultLease = 60 * time.Second
		runner.HeartbeatInterval = 60 * time.Second
		runner.ReaperInterval = time.Second
	}
	e.startPools()

	firm, err := testsupport.ProvisionFirm("agent-eval")
	if err != nil {
		e.Close()
		return nil, err
	}
	e.firm = firm
	e.router = testsupport.NewRouter()
	log.Infof("Evaluation environment ready (tenant=%s blob=%s proxy=%s)", firm.ID, u.Blob.Backend(), proxy.URL())
	return e, nil
}

// devDatabaseName is the developer database's name (dev.sh, api/.env). The
// evaluation never runs on it: a developer API polling it would claim the
// evaluation's steps.
const devDatabaseName = "nomos"

// databaseName is the database a DSN names, URL or key=value form.
func databaseName(dsn string) string {
	if pu, err := url.Parse(dsn); err == nil && (pu.Scheme == "postgres" || pu.Scheme == "postgresql") {
		return strings.TrimPrefix(pu.Path, "/")
	}
	for _, f := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(f, "dbname="); ok {
			return strings.Trim(v, `'"`)
		}
	}
	return ""
}

// withAppName tags the DSN so ForeignClients can tell our connections apart.
func withAppName(dsn string) string {
	if strings.Contains(dsn, "application_name") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if pu, err := url.Parse(dsn); err == nil {
			q := pu.Query()
			q.Set("application_name", appName)
			pu.RawQuery = q.Encode()
			return pu.String()
		}
	}
	return dsn + " application_name=" + appName
}

// ForeignClients counts other sessions on the evaluation database. Another
// API process polling the same database would claim the evaluation's steps
// and run them with ITS configuration — so a gate run refuses to start.
func ForeignClients() (int, error) {
	var n int64
	err := u.DB.Raw(`SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		AND backend_type = 'client backend' AND application_name <> ?`, appName).Scan(&n).Error
	return int(n), err
}

func (e *Env) startPools() {
	ctx, cancel := context.WithCancel(context.Background())
	e.poolsMu.Lock()
	e.stopPools = cancel
	e.poolsMu.Unlock()
	runner.Start(ctx, e.cfg)
}

// restartPools is the process death of the restart case: every pool stops
// (in-flight steps lose their context, their leases are left to expire) and
// a new set starts, exactly as a new pod would.
func (e *Env) restartPools() {
	e.poolsMu.Lock()
	stop := e.stopPools
	e.poolsMu.Unlock()
	if stop != nil {
		stop()
	}
	time.Sleep(3 * time.Second)
	e.startPools()
	log.Infof("Runner pools restarted (simulated process death)")
}

// Close stops the pools, removes the firm's rows and restores the process.
func (e *Env) Close() {
	e.poolsMu.Lock()
	if e.stopPools != nil {
		e.stopPools()
		e.stopPools = nil
	}
	e.poolsMu.Unlock()
	if e.firm.ID != "" {
		time.Sleep(100 * time.Millisecond)
		testsupport.Cleanup(e.firm.ID, e.firm.UserID)
	}
	if e.restoreGW != nil {
		e.restoreGW()
	}
	if e.proxy != nil {
		e.proxy.Close()
	}
	if e.prevBaseURL != "" {
		os.Setenv("OPENROUTER_BASE_URL", e.prevBaseURL)
	} else {
		os.Unsetenv("OPENROUTER_BASE_URL")
	}
	if e.tmpDir != "" {
		_ = os.RemoveAll(e.tmpDir)
	}
}

// Firm is the evaluation tenant.
func (e *Env) Firm() testsupport.Firm { return e.firm }

// Config is the effective configuration (with the proxy as base URL).
func (e *Env) Config() config.Config { return e.cfg }

// Upstream is the gateway the proxy forwards to.
func (e *Env) Upstream() string { return e.upstream }

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func (e *Env) do(method, path string, body any, out any) (int, error) {
	res := testsupport.Do(e.router, method, path, e.firm.UserID, body)
	if out != nil && res.Code < 300 {
		if err := json.Unmarshal(res.Body.Bytes(), out); err != nil {
			return res.Code, fmt.Errorf("%s %s: decoding: %w", method, path, err)
		}
	}
	if res.Code >= 300 {
		return res.Code, fmt.Errorf("%s %s = %d: %s", method, path, res.Code, clip(res.Body.String(), 300))
	}
	return res.Code, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Preparation: corpus, compétences, dossiers
// ---------------------------------------------------------------------------

// lawFreeClasses are classes whose oracle needs no legal corpus.
var lawFreeClasses = []string{ClassGreeting, ClassStatus, ClassNamedPiece, ClassBordereau, ClassChronology, ClassSynthesis, ClassExplicitSubset}

// NeedsLaw reports whether any planned trial may search the law.
func NeedsLaw(trials []Trial) bool {
	for _, t := range trials {
		if !slices.Contains(lawFreeClasses, t.Case.Class) {
			return true
		}
	}
	return false
}

// SeedCorpus loads the public corpus the way a fresh deployment does at
// boot, synchronously, and reports how much of it there is.
func (e *Env) SeedCorpus(ctx context.Context) (int64, error) {
	services.SeedCorpus(ctx, e.cfg)
	var n int64
	err := u.DB.Table("legal_documents").Where("source <> ? AND source NOT LIKE ?", services.SourceFixtures, "test_%").Count(&n).Error
	return n, err
}

// CorpusSHA256 fingerprints the public corpus snapshot the run used.
func CorpusSHA256() (string, error) {
	rows, err := u.DB.Table("legal_versions v").
		Select("d.source, v.legal_document_id, v.source_version, v.text_sha256").
		Joins("JOIN legal_documents d ON d.id = v.legal_document_id").
		Where("d.source NOT LIKE ?", "test_%").
		Order("d.source, v.legal_document_id, v.source_version").Rows()
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := newHasher()
	n := 0
	for rows.Next() {
		var source, doc, version, text string
		if err := rows.Scan(&source, &doc, &version, &text); err != nil {
			return "", err
		}
		h.add(source, doc, version, text)
		n++
	}
	return fmt.Sprintf("%s (versions=%d)", h.sum(), n), rows.Err()
}

// SkillSHA256 fingerprints the compétences seeded for the evaluation firm
// (the built-ins plus the evaluation's own).
func (e *Env) SkillSHA256() string {
	_, _ = e.do(http.MethodGet, "/api/skills", nil, nil) // seeds the built-ins
	var rows []struct {
		Slug       string
		Version    int
		Definition types.JSONB
	}
	u.DB.Table("skill_versions v").Select("s.slug, v.version, v.definition").
		Joins("JOIN skills s ON s.id = v.skill_id").Where("s.tenant_id = ?", e.firm.ID).
		Order("s.slug, v.version").Scan(&rows)
	h := newHasher()
	for _, r := range rows {
		h.add(r.Slug, fmt.Sprint(r.Version), string(r.Definition))
	}
	return h.sum()
}

// ensureCompetence publishes a manifest compétence for the firm, once; a
// built-in one must already be seeded.
func (e *Env) ensureCompetence(slug string) error {
	if slug == "" || e.competences[slug] {
		return nil
	}
	c, ok := e.manifest.Competences[slug]
	if !ok {
		return fmt.Errorf("compétence %s is not in the manifest", slug)
	}
	if c.Builtin {
		_, _ = e.do(http.MethodGet, "/api/skills", nil, nil) // seeds the built-ins
		var n int64
		u.DB.Table("skills").Where("tenant_id = ? AND slug = ? AND state = ?", e.firm.ID, slug, types.SkillStateActive).Count(&n)
		if n != 1 {
			return fmt.Errorf("built-in compétence %s is not seeded for the evaluation firm", slug)
		}
		e.competences[slug] = true
		return nil
	}
	if _, err := e.do(http.MethodPost, "/api/skills", types.SkillCreateRequest{
		Slug: slug, Name: c.Name, Description: c.Description, Definition: types.JSONB(c.Definition),
	}, nil); err != nil {
		return err
	}
	e.competences[slug] = true
	return nil
}

// dossierFor builds (or reuses) the dossier a trial runs on.
func (e *Env) dossierFor(ctx context.Context, t Trial) (*builtDossier, error) {
	c := t.Case
	st := e.manifest.States[c.State]
	key := c.Dossier + "|" + c.State
	if c.FreshDossier || c.Inject != "" {
		key += "|" + t.ID
	}
	if d, ok := e.dossiers[key]; ok {
		return d, nil
	}
	d := &builtDossier{Key: key, Files: map[string]string{}, ByID: map[string]string{}}

	req := types.DossierCreateRequest{Title: "Dossier vide — évaluation", MatterType: "droit du travail"}
	var fixture DossierFixture
	if !st.Empty {
		fixture = e.manifest.Dossiers[c.Dossier]
		req = types.DossierCreateRequest{Title: fixture.Title, ClientReference: fixture.ClientReference, MatterType: fixture.MatterType}
	}
	var dossier types.Dossier
	if _, err := e.do(http.MethodPost, "/api/dossiers", req, &dossier); err != nil {
		return nil, err
	}
	d.ID = dossier.ID

	if !st.Empty {
		var paths []string
		for _, f := range fixture.Files {
			if st.Dossier == c.Dossier && slices.Contains(st.Withhold, f.Name) {
				continue
			}
			paths = append(paths, filepath.Join(e.opts.FixtureRoot, fixture.Dir, f.Name))
		}
		if c.Inject != "" {
			p, err := e.writeGenerated(c.Inject, t.ID)
			if err != nil {
				return nil, err
			}
			paths = append(paths, p)
		}
		if err := e.upload(ctx, d, paths); err != nil {
			return nil, err
		}
	}
	e.dossiers[key] = d
	log.Infof("Dossier ready (key=%s pieces=%d failed=%d)", key, len(d.Files), len(d.Failed))
	return d, nil
}

// writeGenerated writes a generated document to the scratch area and
// returns its path.
func (e *Env) writeGenerated(key, nonce string) (string, error) {
	g, ok := e.manifest.Generated[key]
	if !ok {
		return "", fmt.Errorf("generated document %s is not in the manifest", key)
	}
	raw, err := buildDOCX(g, nonce)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "nomos-eval-gen-")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, g.Filename)
	return p, os.WriteFile(p, raw, 0o644)
}

// upload versés the files and, unless wait is false, waits until ingestion
// settled every one of them.
func (e *Env) upload(ctx context.Context, d *builtDossier, paths []string) error {
	ids, runID, err := e.uploadNoWait(d, paths)
	if err != nil {
		return err
	}
	if runID != "" {
		e.ingestRuns = append(e.ingestRuns, runID)
	}
	deadline := time.Now().Add(e.opts.IngestDeadline)
	for {
		var page struct {
			Items []types.Piece `json:"items"`
		}
		if _, err := e.do(http.MethodGet, "/api/dossiers/"+d.ID+"/pieces?limit=200", nil, &page); err != nil {
			return err
		}
		settled, failed := 0, []string{}
		for _, p := range page.Items {
			if !slices.Contains(ids, p.ID) {
				continue
			}
			switch p.ClassificationState {
			case types.PieceStateProposed, types.PieceStateApproved:
				settled++
			case types.PieceStateFailed, types.PieceStateRejected:
				settled++
				failed = append(failed, d.ByID[p.ID])
			}
		}
		if settled == len(ids) {
			d.Failed = append(d.Failed, failed...)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ingestion did not settle within %s (%d/%d)", e.opts.IngestDeadline, settled, len(ids))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.opts.PollEvery):
		}
	}
}

// uploadNoWait versés the files and returns the new piece ids and the
// ingestion run reading them.
func (e *Env) uploadNoWait(d *builtDossier, paths []string) ([]string, string, error) {
	res, err := testsupport.Upload(e.router, "/api/dossiers/"+d.ID+"/pieces", e.firm.UserID, paths)
	if err != nil {
		return nil, "", err
	}
	if res.Code != http.StatusCreated {
		return nil, "", fmt.Errorf("POST /pieces = %d: %s", res.Code, clip(res.Body.String(), 300))
	}
	var up types.PieceUploadResponse
	if err := json.Unmarshal(res.Body.Bytes(), &up); err != nil {
		return nil, "", err
	}
	if len(up.Rejected) > 0 || len(up.Accepted) != len(paths) {
		reasons := []string{}
		for _, r := range up.Rejected {
			reasons = append(reasons, r.Name+": "+r.Reason)
		}
		return nil, "", fmt.Errorf("upload accepted %d of %d documents (%s)", len(up.Accepted), len(paths), strings.Join(reasons, "; "))
	}
	var ids []string
	for _, p := range up.Accepted {
		d.Files[p.OriginalName] = p.ID
		d.ByID[p.ID] = p.OriginalName
		ids = append(ids, p.ID)
	}
	return ids, up.RunID, nil
}

// IngestionRuns are the runs that read the suite's dossiers.
func (e *Env) IngestionRuns() []string { return e.ingestRuns }

// ---------------------------------------------------------------------------
// One trial
// ---------------------------------------------------------------------------

// trialRun is the mutable state of one trial while it runs.
type trialRun struct {
	t       Trial
	d       *builtDossier
	convID  string
	res     types.MessageCreateResponse
	runID   string
	start   time.Time
	status  string // incomplete_budget | incomplete_timeout when the harness stopped it
	asked   bool
	rounds  int
	acted   bool
	newRuns []string
}

// RunTrial executes one trial and judges it. It never returns an error: a
// trial that could not be executed is a harness_error result, counted in the
// denominator like every other.
func (e *Env) RunTrial(ctx context.Context, t Trial, budget *Budget, suiteDeadline time.Time) (res TrialResult) {
	res = TrialResult{ID: t.ID, Suite: t.Suite, CaseID: t.Case.ID, Class: t.Case.Class, State: t.Case.State,
		Kind: t.Case.Kind, N: t.N, StartedAt: time.Now().UTC()}
	defer func() { res.FinishedAt = time.Now().UTC() }()
	harness := func(err error) TrialResult {
		res.Status, res.Error = StatusHarnessError, err.Error()
		log.Warnf("Trial harness error (trial=%s): %v", t.ID, err)
		return res
	}

	if err := e.ensureCompetence(t.Case.Competence); err != nil {
		return harness(err)
	}
	d, err := e.dossierFor(ctx, t)
	if err != nil {
		return harness(fmt.Errorf("dossier: %w", err))
	}
	before, err := e.runIDs()
	if err != nil {
		return harness(err)
	}

	tr := &trialRun{t: t, d: d, start: time.Now()}
	e.proxy.BeginTrial(t.Case.Faults)
	endTap := sync.OnceValue(e.proxy.EndTrial)
	defer endTap()

	if err := e.startTurn(tr); err != nil {
		return harness(err)
	}
	deadline := tr.start.Add(e.opts.TrialDeadline)
	if suiteDeadline.Before(deadline) {
		deadline = suiteDeadline
	}
	if err := e.drive(ctx, tr, budget, deadline); err != nil {
		return harness(err)
	}
	latency := time.Since(tr.start)
	calls := endTap()

	after, err := e.runIDs()
	if err != nil {
		return harness(err)
	}
	for id := range after {
		if !before[id] {
			tr.newRuns = append(tr.newRuns, id)
		}
	}
	o, err := e.collect(ctx, tr)
	if err != nil {
		return harness(fmt.Errorf("collecting the outcome: %w", err))
	}
	o.LatencyMS = latency.Milliseconds()
	res.Outcome = &o

	usage, err := CollectUsage(u.DB, "l.run_id IN ?", tr.newRuns)
	if err != nil {
		return harness(err)
	}
	res.Usage = usage
	res.ReturnedModels = ReturnedModels(calls)
	res.ModelIssues = ModelIssues(e.opts.Release, calls)
	res.ProxyCalls = len(calls)
	prompts := map[string]bool{}
	ok2xx := 0
	for _, c := range calls {
		if c.Scripted && c.Status < 300 {
			res.ScriptedCalls++
		}
		if c.PromptSHA256 != "" {
			prompts[c.PromptSHA256] = true
		}
		if c.Status >= 200 && c.Status < 300 {
			ok2xx++
		}
	}
	for p := range prompts {
		res.PromptSHA256 = append(res.PromptSHA256, p)
	}
	sort.Strings(res.PromptSHA256)
	res.LegalRefusals, res.PieceWarnings = e.verificationTallies(tr.runID, o)
	if t.Case.Oracle.GoldEvents != "" {
		r := GoldRecall(e.manifest.Gold[t.Case.Oracle.GoldEvents], o)
		res.GoldRecall = &r
	}

	for _, c := range calls {
		if c.WebQuery != "" {
			o.ForwardedQueries = append(o.ForwardedQueries, c.WebQuery)
		}
	}
	res.Failures = Evaluate(t.Case, o, e.manifest)
	// Every completed provider exchange must be a reconciled llm_calls row:
	// a call the product did not record is spend nobody can attribute.
	if ok2xx != usage.Succeeded {
		res.Failures = append(res.Failures, Failure{Check: "accounting",
			Detail: fmt.Sprintf("the proxy saw %d completed exchanges, llm_calls records %d succeeded calls", ok2xx, usage.Succeeded)})
	}
	if err := e.writeOutputs(&res, tr, o); err != nil {
		return harness(fmt.Errorf("writing outputs: %w", err))
	}

	switch {
	case tr.status != "":
		res.Status = tr.status
	case len(res.Failures) > 0 || len(res.ModelIssues) > 0:
		res.Status = StatusFailed
	case usage.UnknownUsage > 0:
		res.Status = StatusUnknownUsage
	default:
		res.Status = StatusPassed
	}
	log.Infof("Trial done (trial=%s status=%s outcome=%s dispatches=%d failures=%d cost=%.4f unknown=%d latency=%s)",
		t.ID, res.Status, o.Outcome, o.Dispatches, len(res.Failures), usage.CostUSD, usage.UnknownUsage, latency.Round(time.Second))
	return res
}

func (e *Env) runIDs() (map[string]bool, error) {
	var ids []string
	if err := u.DB.Table("runs").Where("tenant_id = ?", e.firm.ID).Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// startTurn opens the thread and sends the instruction — with the pending
// pièce of the unreadable/pending state attached while it is still read.
func (e *Env) startTurn(tr *trialRun) error {
	c := tr.t.Case
	var conv types.Conversation
	if _, err := e.do(http.MethodPost, "/api/conversations", types.ConversationCreateRequest{DossierID: &tr.d.ID}, &conv); err != nil {
		return err
	}
	tr.convID = conv.ID
	body := types.MessageCreateRequest{Content: c.Instruction}
	if c.Competence != "" {
		body.SkillSlugs = []string{c.Competence}
	}
	if st := e.manifest.States[c.State]; st.AttachLate != "" && st.Dossier == c.Dossier {
		p, err := e.writeGenerated(st.AttachLate, tr.t.ID)
		if err != nil {
			return err
		}
		ids, _, err := e.uploadNoWait(tr.d, []string{p})
		if err != nil {
			return fmt.Errorf("pending attachment: %w", err)
		}
		body.AttachmentIDs = ids
	}
	if _, err := e.do(http.MethodPost, "/api/conversations/"+conv.ID+"/messages", body, &tr.res); err != nil {
		return err
	}
	if tr.res.Agent == nil || tr.res.RunID == nil {
		return errors.New("the turn did not enter the agent runtime")
	}
	tr.runID = *tr.res.RunID
	return nil
}

// drive polls the run to its end: it answers or records questions, fires the
// case's action, and stops the run on budget or deadline.
func (e *Env) drive(ctx context.Context, tr *trialRun, budget *Budget, deadline time.Time) error {
	c := tr.t.Case
	poll := e.opts.PollEvery
	if c.Action != nil && c.Action.At == "gate" {
		poll = min(poll, 100*time.Millisecond)
	}
	cancelled := false
	stop := func(status string) error {
		tr.status = status
		cancelled = true
		_, err := e.do(http.MethodPost, "/api/runs/"+tr.runID+"/cancel", nil, nil)
		return err
	}
	lastBudget := time.Time{}
	for {
		var detail types.RunDetail
		if _, err := e.do(http.MethodGet, "/api/runs/"+tr.runID, nil, &detail); err != nil {
			return err
		}
		if types.TerminalRunState(detail.State) {
			return nil
		}
		if !cancelled {
			if detail.State == types.RunStateWaitingInput {
				maxRounds := 0
				if c.Answers != nil {
					maxRounds = max(c.Answers.MaxRounds, 1)
				}
				if tr.rounds < maxRounds {
					if err := e.answer(tr, detail, c.Answers.Text); err != nil {
						return err
					}
					tr.rounds++
				} else {
					// The asking is the outcome; stop paying for the rest.
					tr.asked, cancelled = true, true
					if _, err := e.do(http.MethodPost, "/api/runs/"+tr.runID+"/cancel", nil, nil); err != nil {
						return err
					}
				}
			}
			if err := e.act(tr, detail); err != nil {
				return err
			}
			if time.Since(lastBudget) >= 5*time.Second || e.opts.FastRunner {
				lastBudget = time.Now()
				if done, spent, err := budget.Exhausted(); err != nil {
					return err
				} else if done {
					log.Warnf("Budget exhausted during a trial (trial=%s spent=%.4f limit=%.2f)", tr.t.ID, spent, budget.LimitUSD)
					if err := stop(StatusIncompleteBudget); err != nil {
						return err
					}
				}
			}
			if !cancelled && time.Now().After(deadline) {
				log.Warnf("Trial deadline reached (trial=%s)", tr.t.ID)
				if err := stop(StatusIncompleteTimeout); err != nil {
					return err
				}
			}
		} else if time.Now().After(deadline.Add(5 * time.Minute)) {
			return errors.New("the run did not stop within five minutes of its cancellation")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// act fires the case's runner-level action, once.
func (e *Env) act(tr *trialRun, detail types.RunDetail) error {
	a := tr.t.Case.Action
	if a == nil || tr.acted {
		return nil
	}
	switch a.Kind {
	case ActionCancel, ActionRestart:
		var turn types.AgentTurn
		if err := u.ForTenantID(context.Background(), e.firm.ID).AgentTurns().Where("run_id = ?", tr.runID).Take(&turn).Error; err != nil {
			return nil
		}
		if turn.Dispatches < a.AfterDispatch {
			return nil
		}
		tr.acted = true
		// The interruption lands between provider calls, never across one:
		// a dispatch is reserved before its call, so the dispatch counter
		// alone is reached while that call is in flight, and cutting it
		// leaves its spend unknown — a trial that can never pass. The fence
		// answers any new call with a status while the ones in flight
		// finish and are reconciled.
		e.proxy.Fence(true)
		defer e.proxy.Fence(false)
		e.waitQuiet(tr)
		if a.Kind == ActionCancel {
			if _, err := e.do(http.MethodPost, "/api/runs/"+tr.runID+"/cancel", nil, nil); err != nil {
				return err
			}
			// A step already running ends against the fence.
			e.waitQuiet(tr)
			return nil
		}
		e.restartPools()
	case ActionMutate:
		for _, s := range detail.Steps {
			if strings.HasPrefix(s.StepKey, "agent:gate:") && (s.State == types.StepStateRunning || s.State == types.StepStateSucceeded) {
				tr.acted = true
				p, err := e.writeGenerated(a.File, tr.t.ID+"-mutation")
				if err != nil {
					return err
				}
				_, _, err = e.uploadNoWait(tr.d, []string{p})
				log.Infof("Dossier mutated at the final gate (trial=%s step=%s state=%s)", tr.t.ID, s.StepKey, s.State)
				return err
			}
		}
	}
	return nil
}

// citationIdentity names exactly what a published citation points to, as
// the lawyer must check it: the pièce, the article version and its
// validity, the decision, the page.
func citationIdentity(ci types.AgentCitation) string {
	day := func(sec *int64) string {
		if sec == nil {
			return "…"
		}
		return time.Unix(*sec, 0).UTC().Format("2006-01-02")
	}
	var parts []string
	add := func(prefix, v string) {
		if strings.TrimSpace(v) != "" {
			parts = append(parts, prefix+v)
		}
	}
	switch ci.SourceType {
	case types.CitationSourcePiece:
		add("pièce n° ", fmt.Sprint(ci.PieceOrdinal))
		add("", ci.Filename)
	case types.CitationSourceLegifrance:
		add("", ci.ArticleLabel)
		add("version officielle ", ci.OfficialVersionID)
		add("version stockée ", ci.StoredVersionID)
		parts = append(parts, "en vigueur du "+day(ci.ValidFrom)+" au "+day(ci.ValidTo))
		if ci.ApplicabilityDate != nil {
			parts = append(parts, "appliquée au "+day(ci.ApplicabilityDate))
		}
		add("", ci.URL)
	case types.CitationSourceJudilibre:
		add("", ci.Court)
		add("", ci.Chamber)
		if ci.DecisionDate != nil {
			parts = append(parts, day(ci.DecisionDate))
		}
		add("", ci.ECLI)
		add("pourvoi ", ci.Pourvoi)
		add("décision ", ci.DecisionID)
		add("", ci.URL)
	default:
		add("", ci.Title)
		add("", ci.URL)
	}
	return strings.Join(parts, " · ")
}

// addPublicQuery files the query of one search_* call's final activity row.
func (o *Outcome) addPublicQuery(p types.AgentActivityPayload) {
	if p.Object.Kind != types.ActivityObjectQuery ||
		(p.Tool != types.ToolSearchWeb && p.Tool != types.ToolSearchLaw && p.Tool != types.ToolSearchCaselaw) {
		return
	}
	switch p.Code {
	case types.ToolCodeUnsafePublicQuery:
		// Kept inside the cabinet by the confidentiality gate: an
		// observation, not a leak.
		o.RejectedQueries++
	case types.ToolCodeBudgetExhausted:
		// Refused before any dispatch.
	default:
		// Dispatched, or possibly so (a connector failure, an
		// interruption): it may have left the cabinet.
		o.PublicQueries = append(o.PublicQueries, p.Object.Label)
	}
}

// quietWait bounds how long an interruption waits for the trial's model
// calls to settle. Past it the action fires anyway, and a call it cuts is
// reported as unknown usage, never priced.
var quietWait = 3 * time.Minute

// waitQuiet waits until no model call of the trial is in flight.
func (e *Env) waitQuiet(tr *trialRun) {
	deadline := time.Now().Add(quietWait)
	for !e.quiet(tr.runID) {
		if time.Now().After(deadline) {
			log.Warnf("No quiet point before the interruption (trial=%s): a call in flight may end with unknown usage", tr.t.ID)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// quiet reports that the proxy holds no request and every llm_calls row of
// the run is reconciled — a row reserved whose request has not reached the
// proxy yet is a call in flight.
func (e *Env) quiet(runID string) bool {
	if e.proxy.InFlight() > 0 {
		return false
	}
	var open int64
	if err := u.DB.Table("llm_calls").Where("run_id = ? AND state NOT IN ?", runID,
		[]string{types.LLMCallStateSucceeded, types.LLMCallStateFailed}).Count(&open).Error; err != nil || open > 0 {
		return false
	}
	return e.proxy.InFlight() == 0
}

// answer replies to every pending question the way a lawyer would: the first
// option of a choice, the case's text for a free question.
func (e *Env) answer(tr *trialRun, detail types.RunDetail, text string) error {
	var answers []types.QuestionAnswer
	for _, q := range detail.Questions {
		if q.State != types.QuestionStatePending {
			continue
		}
		a := types.QuestionAnswer{QuestionKey: q.QuestionKey}
		var options []struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(q.Options, &options)
		if (q.Kind == types.QuestionKindSingle || q.Kind == types.QuestionKindMulti) && len(options) > 0 {
			a.Values = []string{options[0].Value}
		} else {
			a.Text = text
		}
		answers = append(answers, a)
	}
	if len(answers) == 0 {
		return nil
	}
	_, err := e.do(http.MethodPost, "/api/runs/"+tr.runID+"/answers", types.AnswersRequest{Answers: answers}, nil)
	log.Infof("Questions answered (trial=%s answers=%d round=%d)", tr.t.ID, len(answers), tr.rounds+1)
	return err
}

// ---------------------------------------------------------------------------
// Collection
// ---------------------------------------------------------------------------

var reArticle = regexp.MustCompile(`(?i)\b([LRD])\.?\s*([0-9]+(?:-[0-9]+)+)`)

// collect reads the trial's complete outcome from the durable rows and the
// public endpoints.
func (e *Env) collect(ctx context.Context, tr *trialRun) (Outcome, error) {
	repo := u.ForTenantID(ctx, e.firm.ID)
	o := Outcome{RunID: tr.runID, Tools: map[string]*ToolFacts{}, EvidencePieces: map[string]string{}}

	var run types.Run
	if err := repo.Runs().Where("id = ?", tr.runID).Take(&run).Error; err != nil {
		return o, err
	}
	o.RunState, o.FailureCode = run.State, run.FailureCode
	var turn types.AgentTurn
	if err := repo.AgentTurns().Where("run_id = ?", tr.runID).Take(&turn).Error; err != nil {
		return o, err
	}
	o.TurnOutcome, o.Dispatches = turn.Outcome, turn.Dispatches
	o.ReviewCalls, o.WebSearches = turn.ReviewCalls, turn.WebSearches
	o.PromptVersion, o.ToolsetVersion = turn.PromptVersion, turn.ToolsetVersion
	var contract types.AgentTaskContract
	_ = json.Unmarshal(turn.TaskContract, &contract)
	o.Exhaustive = contract.Exhaustive
	o.Outcome = turn.Outcome
	if tr.asked {
		o.Outcome = OutcomeAsked
	}
	var controller int64
	repo.LLMCalls().Where("run_id = ? AND purpose = ?", tr.runID, "agent").Count(&controller)
	o.ControllerCalls = int(controller)

	// The delivered answer.
	var msg types.Message
	if err := repo.Messages().Where("id = ?", tr.res.AssistantMessage.ID).Take(&msg).Error; err != nil {
		return o, err
	}
	o.Answer = DeliveredAnswer{Text: msg.Content, Chars: len([]rune(msg.Content)), State: msg.State, AgentOutcome: msg.AgentOutcome}
	if msg.AnswerRevisionID != nil {
		o.Answer.RevisionID = *msg.AnswerRevisionID
	}
	if msg.VerificationID != nil {
		o.Answer.VerificationID = *msg.VerificationID
	}
	var completes int64
	repo.RunEvents().Where("conversation_id = ? AND type = ? AND payload->>'message_id' = ?",
		tr.convID, types.EventMessageComplete, msg.ID).Count(&completes)
	o.Answer.CompletedEvents = int(completes)

	if o.Answer.VerificationID != "" {
		var v types.AgentVerification
		if err := repo.AgentVerifications().Where("id = ?", o.Answer.VerificationID).Take(&v).Error; err == nil {
			o.Verification = &VerificationFacts{Result: v.Result, DownloadAllowed: v.DownloadAllowed, Warnings: v.Warnings,
				Blocking: v.Blocking, ScopeSHA256: v.ScopeSHA256, Issues: map[string]int{}}
			var rep types.VerificationReport
			if json.Unmarshal(v.Report, &rep) == nil {
				for _, is := range rep.Issues {
					o.Verification.Issues[is.Code]++
				}
			}
		}
	}

	// Coverage, recomputed now: its scope hash is the dossier as it stands.
	if cov, err := services.ComputeCoverage(ctx, repo, tr.runID); err == nil {
		f := &CoverageFacts{Complete: cov.Complete, Exhaustive: cov.Exhaustive, TotalPieces: cov.TotalPieces,
			FullyRead: cov.FullyRead, Segments: cov.Segments, States: map[string]int{}, ScopeSHA256: cov.ScopeSHA256}
		for _, p := range cov.Pieces {
			f.States[p.State]++
		}
		o.Coverage, o.LiveScopeSHA256 = f, cov.ScopeSHA256
	}

	// Evidence spans → fixture files, and → the legal version they quote.
	var spans []types.AgentEvidenceSpan
	repo.AgentEvidenceSpans().Where("run_id = ?", tr.runID).Find(&spans)
	legalVersion := map[string]string{}
	for _, s := range spans {
		if s.PieceID != nil {
			o.EvidencePieces[s.ID] = firstNonEmpty(tr.d.ByID[*s.PieceID], "piece:"+*s.PieceID)
		}
		if s.LegalVersionID != nil {
			legalVersion[s.ID] = *s.LegalVersionID
		}
	}

	// Revisions and artifacts.
	var artifacts []types.Artifact
	repo.Artifacts().Where("run_id = ?", tr.runID).Order("created_at").Find(&artifacts)
	published := map[string]bool{o.Answer.RevisionID: o.Answer.RevisionID != ""}
	for _, a := range artifacts {
		if a.DocumentRevisionID != nil {
			published[*a.DocumentRevisionID] = true
		}
	}
	var revs []types.AgentDocumentRevision
	repo.AgentDocumentRevisions().Where("run_id = ?", tr.runID).Order("document_key, revision").Find(&revs)
	kinds := map[string]string{}
	for _, r := range revs {
		f := RevisionFacts{ID: r.ID, DocumentKey: r.DocumentKey, Revision: r.Revision, Kind: r.Kind,
			ContentSHA256: r.ContentSHA256, Published: published[r.ID]}
		_ = json.Unmarshal(r.Body, &f.Body)
		o.Revisions = append(o.Revisions, f)
		kinds[r.ID] = r.Kind
	}
	for _, a := range artifacts {
		f := ArtifactFacts{ID: a.ID, Kind: a.Kind, Name: a.Name, Partial: a.Partial, PieceWarnings: a.PieceWarnings}
		if a.DocumentRevisionID != nil {
			f.RevisionID, f.RevisionKind = *a.DocumentRevisionID, kinds[*a.DocumentRevisionID]
		}
		f.DownloadStatus, _ = e.do(http.MethodGet, "/api/artifacts/"+a.ID+"/download", nil, nil)
		if raw, err := e.artifactBytes(ctx, a.AssetID); err == nil {
			f.SHA256, f.Size = sha256Hex(raw), int64(len(raw))
			if strings.HasSuffix(strings.ToLower(a.Name), ".docx") {
				f.Text, _ = docxText(raw)
			}
			f.Path = "" // set by writeOutputs
			artifactCache.Store(a.ID, raw)
		}
		o.Artifacts = append(o.Artifacts, f)
	}

	// Citations, as the thread shows them.
	after := ""
	for page := 0; page < 50; page++ {
		var list types.AgentCitationList
		if _, err := e.do(http.MethodGet, "/api/runs/"+tr.runID+"/citations?limit=100"+after, nil, &list); err != nil {
			break
		}
		for _, ci := range list.Items {
			loc := ci.Locator
			f := CitationFacts{Number: ci.Number, SourceType: ci.SourceType, Label: ci.Label, Identity: citationIdentity(ci),
				Quote: ci.Quote, Locator: &loc, Checks: map[string]string{}, OnAnswer: ci.MessageID != nil && *ci.MessageID == msg.ID}
			if ci.PieceID != "" {
				f.PieceFile = firstNonEmpty(tr.d.ByID[ci.PieceID], "piece:"+ci.PieceID)
			}
			if ci.SourceType == types.CitationSourceLegifrance {
				f.Article = firstNonEmpty(nomoseval.ArticleOf(ci.ArticleLabel), nomoseval.ArticleOf(ci.Label), articleOf(ci.ArticleLabel+" "+ci.Label))
			}
			if ci.SourceType == types.CitationSourceLegifrance || ci.SourceType == types.CitationSourceJudilibre {
				var v *types.LegalVersion
				if id := legalVersion[ci.EvidenceSpanID]; id != "" {
					var row types.LegalVersion
					if u.DB.WithContext(ctx).Where("id = ?", id).Take(&row).Error == nil {
						v = &row
					}
				}
				f.Corpus = firstNonEmpty(LegalCitationProblem(ci, v), CorpusResolved)
			}
			for _, ch := range ci.Checks {
				f.Checks[ch.Check] = ch.Status
			}
			o.Citations = append(o.Citations, f)
		}
		if list.NextAfter == nil {
			break
		}
		after = fmt.Sprintf("&after=%d", *list.NextAfter)
	}

	// Activity rows: the final state of every call.
	var events []types.RunEvent
	repo.RunEvents().Where("conversation_id = ? AND type = ?", tr.convID, types.EventAgentActivity).Order("id").Find(&events)
	final := map[string]types.AgentActivityPayload{}
	for _, ev := range events {
		var p types.AgentActivityPayload
		if json.Unmarshal(ev.Payload, &p) == nil {
			final[p.CallID+"|"+p.Tool] = p
		}
	}
	for _, p := range final {
		tf := o.Tools[p.Tool]
		if tf == nil {
			tf = &ToolFacts{}
			o.Tools[p.Tool] = tf
		}
		tf.Calls++
		switch p.Status {
		case types.ActivityOK:
			tf.OK++
		case types.ActivityEmpty:
			tf.Empty++
		case types.ActivityError:
			tf.Error++
			o.ErrorRows++
		}
		o.addPublicQuery(p)
	}
	sort.Strings(o.PublicQueries)

	// Questions.
	var qs []types.RunQuestion
	repo.RunQuestions().Where("run_id = ?", tr.runID).Order("asked_at, question_key").Find(&qs)
	for _, q := range qs {
		o.Questions = append(o.Questions, QuestionFacts{Key: q.QuestionKey, Kind: q.Kind, Required: q.Required,
			Answered: q.AnsweredAt != nil, Prompt: q.Prompt})
	}
	o.AnswerRounds = tr.rounds

	// Decisions the server refused (corrective dispatches).
	var steps []types.RunStep
	repo.RunSteps().Where("run_id = ? AND step_key LIKE ?", tr.runID, "agent:decision:%").Find(&steps)
	for _, s := range steps {
		var rec struct {
			Rejection string         `json:"rejection"`
			Prefailed map[string]any `json:"prefailed"`
			Unusable  string         `json:"unusable"`
		}
		if len(s.Output) > 0 && json.Unmarshal(s.Output, &rec) == nil &&
			(rec.Rejection != "" || len(rec.Prefailed) > 0 || rec.Unusable != "") {
			o.Rejections++
		}
	}
	return o, nil
}

func articleOf(s string) string {
	m := reArticle.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + m[2]
}

// artifactCache holds artifact bytes between collection and output writing.
var artifactCache sync.Map

func (e *Env) artifactBytes(ctx context.Context, assetID string) ([]byte, error) {
	var asset types.Asset
	if err := u.ForTenantID(ctx, e.firm.ID).Assets().Where("id = ?", assetID).Take(&asset).Error; err != nil {
		return nil, err
	}
	rc, err := u.Blob.Get(ctx, asset.S3Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 64<<20))
}

// verificationTallies counts the run's legal refusals (blocking legal issues
// in any verification the gate stored) and the pièce warnings delivered.
func (e *Env) verificationTallies(runID string, o Outcome) (legal, warnings int) {
	var vs []types.AgentVerification
	u.ForTenantID(context.Background(), e.firm.ID).AgentVerifications().Where("run_id = ?", runID).Find(&vs)
	for _, v := range vs {
		var rep types.VerificationReport
		if json.Unmarshal(v.Report, &rep) != nil {
			continue
		}
		for _, is := range rep.Issues {
			if is.Check == types.CheckLegalCitations && is.Severity == types.SeverityBlocking {
				legal++
			}
		}
	}
	if o.Verification != nil {
		warnings = o.Verification.Warnings
	}
	for _, a := range o.Artifacts {
		warnings += a.PieceWarnings
	}
	return legal, warnings
}

// ---------------------------------------------------------------------------
// Outputs for the lawyer's review
// ---------------------------------------------------------------------------

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// locatorText is a locator as the lawyer reads it.
func locatorText(l types.SourceLocator) string {
	var parts []string
	if l.Page > 0 {
		parts = append(parts, fmt.Sprintf("page %d", l.Page))
	}
	if l.Paragraph > 0 {
		parts = append(parts, fmt.Sprintf("paragraphe %d", l.Paragraph))
	}
	if l.Sheet != "" {
		parts = append(parts, "feuille "+l.Sheet)
	}
	if l.Cell != "" {
		parts = append(parts, "cellule "+l.Cell)
	}
	if l.End > l.Start {
		parts = append(parts, fmt.Sprintf("caractères %d–%d", l.Start, l.End))
	}
	return strings.Join(parts, ", ")
}

// OutputsDir is where a report's reviewable outputs live.
func OutputsDir(reportPath string) string {
	return strings.TrimSuffix(reportPath, filepath.Ext(reportPath)) + ".outputs"
}

// writeOutputs saves what the lawyer must review — the delivered answer, the
// questions asked and every published file — and binds them with one hash.
func (e *Env) writeOutputs(res *TrialResult, tr *trialRun, o Outcome) error {
	type art struct {
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
	}
	type question struct {
		Key    string `json:"key"`
		Kind   string `json:"kind"`
		Prompt string `json:"prompt"`
	}
	// What each [n] points to is part of what the lawyer approves: the hash
	// binds every published citation, not only the text carrying its marker.
	type citation struct {
		Number   int                  `json:"number"`
		OnAnswer bool                 `json:"on_answer"`
		Source   string               `json:"source_type"`
		Label    string               `json:"label"`
		Identity string               `json:"identity"`
		Locator  *types.SourceLocator `json:"locator,omitempty"`
		Quote    string               `json:"quote"`
		Checks   map[string]string    `json:"checks"`
	}
	bound := struct {
		Trial     string     `json:"trial"`
		Outcome   string     `json:"outcome"`
		Answer    string     `json:"answer"`
		Revision  string     `json:"answer_revision_sha256"`
		Citations []citation `json:"citations"`
		Artifacts []art      `json:"artifacts"`
		Questions []question `json:"questions"`
	}{Trial: res.ID, Outcome: o.Outcome, Answer: o.Answer.Text, Citations: []citation{}, Artifacts: []art{}, Questions: []question{}}
	for _, ci := range o.Citations {
		bound.Citations = append(bound.Citations, citation{Number: ci.Number, OnAnswer: ci.OnAnswer, Source: ci.SourceType,
			Label: ci.Label, Identity: ci.Identity, Locator: ci.Locator, Quote: ci.Quote, Checks: ci.Checks})
	}
	sort.SliceStable(bound.Citations, func(i, j int) bool {
		a, b := bound.Citations[i], bound.Citations[j]
		if a.OnAnswer != b.OnAnswer {
			return a.OnAnswer
		}
		return a.Number < b.Number
	})

	for _, r := range o.Revisions {
		if r.ID == o.Answer.RevisionID {
			bound.Revision = r.ContentSHA256
		}
	}
	for _, a := range o.Artifacts {
		bound.Artifacts = append(bound.Artifacts, art{Kind: firstNonEmpty(a.RevisionKind, a.Kind), SHA256: a.SHA256})
	}
	for _, q := range o.Questions {
		bound.Questions = append(bound.Questions, question{Key: q.Key, Kind: q.Kind, Prompt: q.Prompt})
	}
	res.OutputSHA256 = canonicalSHA256(bound)

	if e.opts.ReportPath == "" {
		for _, a := range o.Artifacts {
			artifactCache.Delete(a.ID)
		}
		return nil
	}
	dir := filepath.Join(OutputsDir(e.opts.ReportPath), reUnsafe.ReplaceAllString(res.ID, "_"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	res.OutputDir = dir
	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\noutcome: %s\noutput_sha256: %s\n\n## Réponse délivrée\n\n%s\n", res.ID, o.Outcome, res.OutputSHA256, o.Answer.Text)
	if len(bound.Citations) > 0 {
		md.WriteString("\n## Sources citées\n\n")
		for _, c := range bound.Citations {
			where := "réponse"
			if !c.OnAnswer {
				where = "document"
			}
			fmt.Fprintf(&md, "- [%d] (%s, %s) %s — %s", c.Number, c.Source, where, c.Label, c.Identity)
			if c.Locator != nil {
				if l := locatorText(*c.Locator); l != "" {
					md.WriteString(" — " + l)
				}
			}
			fmt.Fprintf(&md, "\n  > %s\n", strings.Join(strings.Fields(c.Quote), " "))
			checks := make([]string, 0, len(c.Checks))
			for k, v := range c.Checks {
				checks = append(checks, k+"="+v)
			}
			sort.Strings(checks)
			fmt.Fprintf(&md, "  contrôles : %s\n", strings.Join(checks, ", "))
		}
	}
	if len(o.Questions) > 0 {
		md.WriteString("\n## Questions posées\n\n")
		for _, q := range o.Questions {
			fmt.Fprintf(&md, "- [%s, %s, obligatoire=%t, répondue=%t] %s\n", q.Key, q.Kind, q.Required, q.Answered, q.Prompt)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "answer.md"), []byte(md.String()), 0o644); err != nil {
		return err
	}
	for i := range o.Artifacts {
		a := &res.Outcome.Artifacts[i]
		raw, ok := artifactCache.LoadAndDelete(a.ID)
		if !ok {
			continue
		}
		name := fmt.Sprintf("%02d-%s", i+1, reUnsafe.ReplaceAllString(firstNonEmpty(a.Name, a.Kind), "_"))
		if err := os.WriteFile(filepath.Join(dir, name), raw.([]byte), 0o644); err != nil {
			return err
		}
		a.Path = filepath.Join(dir, name)
	}
	raw, err := json.MarshalIndent(bound, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "bound-output.json"), raw, 0o644); err != nil {
		return err
	}
	full, err := json.MarshalIndent(res.Outcome.Record(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "outcome.json"), full, 0o644)
}
