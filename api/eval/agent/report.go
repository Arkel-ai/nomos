package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/templates"
)

// Trial statuses. Only `passed` counts towards a passing report.
const (
	StatusPassed            = "passed"
	StatusFailed            = "failed"
	StatusIncompleteBudget  = "incomplete_budget"
	StatusIncompleteTimeout = "incomplete_timeout"
	StatusUnknownUsage      = "unknown_usage"
	StatusHarnessError      = "harness_error"
	StatusNotRun            = "not_run"
)

// ReportVersion pins the report's JSON shape.
const ReportVersion = 1

// Fingerprint identifies exactly what was evaluated. ConfigFingerprint covers
// everything recomputable from a checkout and the release config, so the
// gate can recompute it at deployment time; the commit pins the code (and so
// the system prompt and the built-in compétences) beside it.
type Fingerprint struct {
	Commit            string `json:"commit"`
	Dirty             bool   `json:"dirty"`
	ConfigSHA256      string `json:"config_sha256"`
	ToolSchemaSHA256  string `json:"tool_schema_sha256"`
	TemplateSHA256    string `json:"template_sha256"`
	VerifierVersion   string `json:"verifier_version"`
	SuiteSHA256       string `json:"suite_sha256"`
	ManifestSHA256    string `json:"fixture_manifest_sha256"`
	CompetenceSHA256  string `json:"eval_competence_sha256"`
	ConfigFingerprint string `json:"config_fingerprint"`
	// Unreviewed names the gold lists the suites use and the compétences of
	// the manifest that no lawyer has signed yet. Their statuses live in the
	// manifest, whose hash is part of ConfigFingerprint: a later sign-off
	// invalidates every report made before it.
	Unreviewed []string `json:"unreviewed,omitempty"`
}

// StaticFingerprint computes the recomputable fingerprint for a release
// config hash, the suites and the manifest.
func StaticFingerprint(configSHA string, suites []Suite, m Manifest) Fingerprint {
	fp := Fingerprint{
		ConfigSHA256:     configSHA,
		ToolSchemaSHA256: ToolSchemaSHA256(),
		TemplateSHA256:   TemplateSHA256(),
		VerifierVersion:  services.VerifierVersionFinal,
		ManifestSHA256:   m.SHA256,
		CompetenceSHA256: canonicalSHA256(m.Competences),
	}
	ids := make([]string, 0, len(suites))
	for _, s := range suites {
		ids = append(ids, s.Suite+":"+s.SHA256)
	}
	sort.Strings(ids)
	fp.SuiteSHA256 = sha256Hex([]byte(strings.Join(ids, "\n")))
	fp.ConfigFingerprint = canonicalSHA256([]string{
		fp.ConfigSHA256, fp.ToolSchemaSHA256, fp.TemplateSHA256, fp.VerifierVersion,
		fp.SuiteSHA256, fp.ManifestSHA256, fp.CompetenceSHA256,
	})
	fp.Unreviewed = UnreviewedMaterial(suites, m)
	fp.Commit, fp.Dirty = GitState()
	return fp
}

// UnreviewedMaterial is the evaluation material a lawyer has not signed:
// every gold list a suite's oracle uses and every manifest compétence whose
// status does not start with "reviewed" (e.g. "reviewed by Me …, 2026-10-20").
func UnreviewedMaterial(suites []Suite, m Manifest) []string {
	reviewed := func(status string) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(status)), "reviewed")
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range suites {
		for _, c := range s.Cases {
			key := c.Oracle.GoldEvents
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			if g, ok := m.Gold[key]; !ok || !reviewed(g.Status) {
				out = append(out, "gold list "+key)
			}
		}
	}
	for slug, c := range m.Competences {
		if !reviewed(c.Status) {
			out = append(out, "compétence "+slug)
		}
	}
	sort.Strings(out)
	return out
}

// ToolSchemaSHA256 hashes the tool catalogue the controller is offered,
// including the server-side scheduling flags the JSON form hides.
func ToolSchemaSHA256() string {
	if services.AgentTools == nil {
		return "no-registry"
	}
	type def struct {
		Name, Description, Class string
		ReadOnly, Exclusive      bool
		Parameters               map[string]any
	}
	var defs []def
	for _, d := range services.AgentTools.Definitions() {
		defs = append(defs, def{d.Name, d.Description, d.Class, d.ReadOnly, d.Exclusive, d.Parameters})
	}
	return canonicalSHA256(defs)
}

// TemplateSHA256 hashes the controlled templates and the reference DOCX.
func TemplateSHA256() string {
	return canonicalSHA256([]string{
		templates.AgentDocumentVersion, templates.AgentDocumentMarkdown,
		templates.DocumentVersion, templates.DocumentMarkdown,
		templates.BordereauVersion, templates.BordereauMarkdown,
		templates.ReferenceSHA256(),
	})
}

// GitState is HEAD and whether the tracked tree differs from it. Untracked
// files (testdata/, which is untracked by design) do not make it dirty.
func GitState() (string, bool) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown", true
	}
	commit := strings.TrimSpace(string(out))
	status, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return commit, true
	}
	return commit, strings.TrimSpace(string(status)) != ""
}

// TrialResult is one trial in the report.
type TrialResult struct {
	ID     string `json:"id"`
	Suite  string `json:"suite"`
	CaseID string `json:"case_id"`
	Class  string `json:"class"`
	State  string `json:"state"`
	Kind   string `json:"kind,omitempty"`
	N      int    `json:"n"`

	Status   string    `json:"status"`
	Failures []Failure `json:"failures,omitempty"`
	Error    string    `json:"error,omitempty"`

	Outcome *Outcome `json:"outcome,omitempty"`
	Usage   Usage    `json:"usage"`
	// ReturnedModels is requested → returned, as the proxy saw every call.
	ReturnedModels map[string]map[string]int `json:"returned_models,omitempty"`
	ModelIssues    []string                  `json:"model_issues,omitempty"`
	ScriptedCalls  int                       `json:"scripted_dispatches"`
	ProxyCalls     int                       `json:"proxy_calls"`
	PromptSHA256   []string                  `json:"prompt_sha256,omitempty"`
	GoldRecall     *float64                  `json:"gold_recall,omitempty"`
	LegalRefusals  int                       `json:"legal_refusals"`
	PieceWarnings  int                       `json:"piece_warnings"`

	OutputSHA256 string `json:"output_sha256,omitempty"`
	OutputDir    string `json:"output_dir,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Counts is the exact trial accounting. Planned is the tier's fixed
// workload; nothing ever shrinks it.
type Counts struct {
	Planned           int `json:"planned"`
	Run               int `json:"run"`
	Passed            int `json:"passed"`
	Failed            int `json:"failed"`
	IncompleteBudget  int `json:"incomplete_budget"`
	IncompleteTimeout int `json:"incomplete_timeout"`
	UnknownUsage      int `json:"unknown_usage"`
	HarnessError      int `json:"harness_error"`
	NotRun            int `json:"not_run"`
}

// Report is the evaluation's whole result.
type Report struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Tier        string    `json:"tier"`
	Suites      []string  `json:"suites"`
	Case        string    `json:"case,omitempty"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	// StoppedBy is why the suite stopped before its workload: a trial status
	// (incomplete_budget, incomplete_timeout) or a harness reason.
	StoppedBy string `json:"stopped_by,omitempty"`

	Fingerprint Fingerprint       `json:"fingerprint"`
	ReleaseName string            `json:"release_name"`
	Effective   map[string]string `json:"effective_config"`
	CorpusSHA   string            `json:"corpus_sha256"`
	SkillSHA    string            `json:"builtin_skill_sha256,omitempty"`
	Prompts     []string          `json:"prompt_sha256"`
	Flags       RunFlags          `json:"flags"`

	Workload Workload       `json:"workload"`
	Counts   Counts         `json:"counts"`
	Outcomes map[string]int `json:"outcomes"`

	ExhaustiveTrials   int     `json:"exhaustive_trials"`
	CompleteCoverage   int     `json:"complete_coverage"`
	GoldTrials         int     `json:"gold_trials"`
	GoldMeanRecall     float64 `json:"gold_mean_recall"`
	LegalRefusals      int     `json:"legal_refusals"`
	PieceWarnings      int     `json:"piece_warnings"`
	DecisionRejections int     `json:"decision_rejections"`
	CapOutcomes        int     `json:"cap_outcomes"`
	DeadlineOutcomes   int     `json:"deadline_outcomes"`
	LatencyP50MS       int64   `json:"latency_p50_ms"`
	LatencyP95MS       int64   `json:"latency_p95_ms"`
	LatencyMaxMS       int64   `json:"latency_max_ms"`
	Artifacts          int     `json:"artifacts"`
	ModelIssues        int     `json:"model_issues"`
	ScriptedDispatches int     `json:"scripted_dispatches"`
	Usage              Usage   `json:"usage"`
	IngestionUsage     Usage   `json:"ingestion_usage"`
	// IngestionModelIssues judges the calls made outside any trial
	// (qualification, OCR, corpus seeding) like a trial's.
	IngestionModelIssues    []string                  `json:"ingestion_model_issues,omitempty"`
	IngestionReturnedModels map[string]map[string]int `json:"ingestion_returned_models,omitempty"`
	BudgetUSD               float64                   `json:"budget_usd"`
	DependencyProblems      []string                  `json:"dependency_problems,omitempty"`
	ProductionProblems      []string                  `json:"production_problems,omitempty"`

	Trials       []TrialResult `json:"trials"`
	ReportSHA256 string        `json:"report_sha256"`
}

// RunFlags records the modes the run was made under; the gate requires all
// three.
type RunFlags struct {
	RequireModels       bool   `json:"require_models"`
	RequireDependencies bool   `json:"require_dependencies"`
	Timeout             string `json:"timeout"`
	// NoDotenv: the run read no .env file — the exported environment was its
	// only configuration.
	NoDotenv bool `json:"no_dotenv"`
}

// Seal computes the report's own hash over everything but the hash.
func (r *Report) Seal() {
	r.ReportSHA256 = ""
	r.ReportSHA256 = canonicalSHA256(r)
}

// Sealed reports whether the stored hash matches the content.
func (r Report) Sealed() bool {
	want := r.ReportSHA256
	r.ReportSHA256 = ""
	return want != "" && canonicalSHA256(r) == want
}

// Aggregate fills the report's totals from its trials. Planned trials that
// never ran stay in the list as not_run: the denominator is the workload.
func (r *Report) Aggregate() {
	r.Counts = Counts{Planned: len(r.Trials)}
	r.Outcomes = map[string]int{}
	var latencies []int64
	var recallSum float64
	r.ExhaustiveTrials, r.CompleteCoverage, r.GoldTrials = 0, 0, 0
	r.LegalRefusals, r.PieceWarnings, r.DecisionRejections = 0, 0, 0
	r.CapOutcomes, r.DeadlineOutcomes, r.Artifacts, r.ScriptedDispatches = 0, 0, 0, 0
	r.ModelIssues = len(r.IngestionModelIssues)
	prompts := map[string]bool{}
	for _, t := range r.Trials {
		switch t.Status {
		case StatusPassed:
			r.Counts.Passed++
		case StatusFailed:
			r.Counts.Failed++
		case StatusIncompleteBudget:
			r.Counts.IncompleteBudget++
		case StatusIncompleteTimeout:
			r.Counts.IncompleteTimeout++
		case StatusUnknownUsage:
			r.Counts.UnknownUsage++
		case StatusHarnessError:
			r.Counts.HarnessError++
		default:
			r.Counts.NotRun++
		}
		if t.Status != StatusNotRun && t.Status != "" {
			r.Counts.Run++
		}
		r.LegalRefusals += t.LegalRefusals
		r.PieceWarnings += t.PieceWarnings
		r.ModelIssues += len(t.ModelIssues)
		r.ScriptedDispatches += t.ScriptedCalls
		for _, p := range t.PromptSHA256 {
			prompts[p] = true
		}
		if t.GoldRecall != nil {
			r.GoldTrials++
			recallSum += *t.GoldRecall
		}
		o := t.Outcome
		if o == nil {
			continue
		}
		r.Outcomes[o.Outcome]++
		if o.Exhaustive {
			r.ExhaustiveTrials++
			if o.Coverage != nil && o.Coverage.Complete {
				r.CompleteCoverage++
			}
		}
		r.DecisionRejections += o.Rejections
		r.Artifacts += len(o.Artifacts)
		switch o.TurnOutcome {
		case "agent_turn_limit":
			r.CapOutcomes++
		case "agent_deadline":
			r.DeadlineOutcomes++
		}
		latencies = append(latencies, o.LatencyMS)
	}
	if r.GoldTrials > 0 {
		r.GoldMeanRecall = recallSum / float64(r.GoldTrials)
	}
	r.Prompts = r.Prompts[:0]
	for p := range prompts {
		r.Prompts = append(r.Prompts, p)
	}
	sort.Strings(r.Prompts)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if n := len(latencies); n > 0 {
		r.LatencyP50MS = latencies[(n-1)/2]
		r.LatencyP95MS = latencies[min(n-1, int(float64(n)*0.95))]
		r.LatencyMaxMS = latencies[n-1]
	}
	r.Status, r.Reason = r.overallStatus()
}

// overallStatus: green only when every planned trial passed. Anything that
// did not run, ran out of money or time, or has unknown usage is never green.
func (r Report) overallStatus() (string, string) {
	c := r.Counts
	switch {
	case c.Failed > 0:
		return StatusFailed, fmt.Sprintf("%d/%d trial(s) missed their oracle", c.Failed, c.Planned)
	case len(r.IngestionModelIssues) > 0:
		return StatusFailed, "ingestion ran on a model the release does not approve: " + strings.Join(r.IngestionModelIssues, "; ")
	case c.HarnessError > 0:
		return StatusHarnessError, fmt.Sprintf("%d trial(s) could not be executed", c.HarnessError)
	case c.IncompleteBudget > 0 || r.StoppedBy == StatusIncompleteBudget:
		return StatusIncompleteBudget, fmt.Sprintf("the $%.2f budget was exhausted with %d trial(s) unfinished or not run", r.BudgetUSD, c.IncompleteBudget+c.NotRun)
	case c.IncompleteTimeout > 0 || r.StoppedBy == StatusIncompleteTimeout:
		return StatusIncompleteTimeout, fmt.Sprintf("%d trial(s) hit the deadline, %d never ran", c.IncompleteTimeout, c.NotRun)
	case c.NotRun > 0:
		return StatusNotRun, fmt.Sprintf("%d planned trial(s) never ran (%s)", c.NotRun, firstNonEmpty(r.StoppedBy, "interrupted"))
	case c.UnknownUsage > 0 || r.Usage.UnknownUsage > 0:
		return StatusUnknownUsage, fmt.Sprintf("%d call(s) have unknown usage and must be reconciled", r.Usage.UnknownUsage)
	case c.Passed != c.Planned || c.Planned == 0:
		return StatusFailed, "the passed count does not equal the planned workload"
	}
	return StatusPassed, ""
}

// WriteReport writes the sealed report.
func WriteReport(path string, r *Report) error {
	r.Seal()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// ReadReport reads a report back.
func ReadReport(path string) (Report, error) {
	var r Report
	raw, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("report %s: %w", path, err)
	}
	return r, nil
}

// Summary is the console digest of a report.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "NOMOS AGENT EVALUATION — tier %s — %s\n", r.Tier, strings.ToUpper(r.Status))
	if r.Reason != "" {
		fmt.Fprintf(&b, "  %s\n", r.Reason)
	}
	fmt.Fprintf(&b, "commit %s (dirty=%t)  config %s\n", short(r.Fingerprint.Commit), r.Fingerprint.Dirty, short(r.Fingerprint.ConfigFingerprint))
	c := r.Counts
	fmt.Fprintf(&b, "trials: planned %d, run %d, passed %d, failed %d, budget %d, timeout %d, unknown usage %d, harness %d, not run %d\n",
		c.Planned, c.Run, c.Passed, c.Failed, c.IncompleteBudget, c.IncompleteTimeout, c.UnknownUsage, c.HarnessError, c.NotRun)
	fmt.Fprintf(&b, "cost: $%.4f known (budget $%.2f), unknown-usage calls %d; ingestion $%.4f\n",
		r.Usage.CostUSD, r.BudgetUSD, r.Usage.UnknownUsage, r.IngestionUsage.CostUSD)
	fmt.Fprintf(&b, "latency: p50 %s, p95 %s, max %s; cap outcomes %d, deadline outcomes %d\n",
		time.Duration(r.LatencyP50MS)*time.Millisecond, time.Duration(r.LatencyP95MS)*time.Millisecond,
		time.Duration(r.LatencyMaxMS)*time.Millisecond, r.CapOutcomes, r.DeadlineOutcomes)
	for _, t := range r.Trials {
		line := fmt.Sprintf("  %-46s %-18s", t.ID, t.Status)
		if t.Outcome != nil {
			line += fmt.Sprintf(" outcome=%s dispatches=%d cost=$%.4f", t.Outcome.Outcome, t.Outcome.Dispatches, t.Usage.CostUSD)
		}
		b.WriteString(line + "\n")
		for _, f := range t.Failures {
			fmt.Fprintf(&b, "      FAIL %s: %s\n", f.Check, f.Detail)
		}
		for _, m := range t.ModelIssues {
			fmt.Fprintf(&b, "      FAIL model: %s\n", m)
		}
		if t.Error != "" {
			fmt.Fprintf(&b, "      FAIL harness: %s\n", t.Error)
		}
	}
	return b.String()
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
