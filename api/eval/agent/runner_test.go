package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The harness's own integration tests: the REAL runtime (Postgres, runner
// pools, tools, verifier, publisher, delivery) driven through the harness,
// with a scripted OpenRouter double behind the recording proxy. Nothing here
// is paid or networked, so it runs under NOMOS_LIVE=0; it needs a disposable
// Postgres (DATABASE_URL), and fails without one under
// NOMOS_REQUIRE_DEPENDENCIES=1.

const fakeCost = 0.0042

// fakeGateway answers controller dispatches like a well-behaved agent on a
// greeting: write the answer revision, then submit it.
type fakeGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	returned string
	usage    bool
	hits     int
	delay    time.Duration // how long each completion takes
}

var reRevisionID = regexp.MustCompile(`"revision_id"\s*:\s*"([A-Za-z0-9_-]+)"`)

func newFakeGateway(t *testing.T, returned string, usage bool) *fakeGateway {
	t.Helper()
	g := &fakeGateway{returned: returned, usage: usage}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.hits++
		delay := g.delay
		g.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		if r.URL.Path != "/chat/completions" || !strings.Contains(string(raw), `"function"`) {
			http.Error(w, `{"error":{"message":"unexpected call in the harness test"}}`, http.StatusBadRequest)
			return
		}
		revision := ""
		for _, m := range body.Messages {
			if s, ok := m.Content.(string); ok && m.Role == "tool" {
				if mm := reRevisionID.FindStringSubmatch(s); mm != nil {
					revision = mm[1]
				}
			}
		}
		var name string
		var args map[string]any
		if revision == "" {
			name, args = types.ToolWriteDocument, map[string]any{
				"progress_note": "Je rédige la réponse.", "document_key": "reponse", "kind": types.AgentOutputAnswer,
				"title": "Réponse", "skill_version_id": nil,
				"blocks": []map[string]any{{"id": nil, "role": types.BlockParagraph, "level": nil,
					"text": "Bonjour Maître, je reste à votre disposition pour ce dossier.", "claims": nil,
					"chronology_date": nil, "bordereau": nil}},
			}
		} else {
			name, args = types.ToolSubmitResult, map[string]any{
				"progress_note": "Je soumets la réponse au contrôle.", "outcome": types.AgentOutcomeCompleted,
				"answer_revision_id": revision, "document_revision_ids": nil, "limitations": nil, "thread_title": nil,
			}
		}
		a, _ := json.Marshal(args)
		resp := map[string]any{
			"id": "gen-" + uuid.NewString()[:8], "model": g.returned,
			"choices": []map[string]any{{"finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []map[string]any{{"id": "call_" + uuid.NewString()[:8], "type": "function",
					"function": map[string]any{"name": name, "arguments": string(a)}}},
			}}},
		}
		if g.usage {
			resp["usage"] = map[string]any{"prompt_tokens": 1200, "completion_tokens": 80, "cost": fakeCost,
				"prompt_tokens_details": map[string]any{"cached_tokens": 600}, "completion_tokens_details": map[string]any{"reasoning_tokens": 40}}
		}
		out, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// bootHarness boots the evaluation environment against the fake gateway.
func bootHarness(t *testing.T, g *fakeGateway, reportPath string) *Env {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		testmode.Missing(t, "DATABASE_URL is not set: the harness's integration tests need a disposable Postgres")
	}
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("PLANNER_MODEL", "openai/gpt-6-luna-pro")
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	env, err := Boot(context.Background(), Options{
		Release: testRelease(), Upstream: g.srv.URL, FastRunner: true, ReportPath: reportPath,
		PollEvery: 40 * time.Millisecond, TrialDeadline: 90 * time.Second, IngestDeadline: 90 * time.Second,
	}, m)
	if err != nil {
		if strings.Contains(err.Error(), "not reachable") {
			testmode.Missing(t, "%v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(env.Close)
	return env
}

func greetingTrial(t *testing.T, n int) Trial {
	t.Helper()
	_, fr, _ := loadAll(t)
	c, ok := fr.Case("c01-s6")
	if !ok {
		t.Fatal("c01-s6 is missing")
	}
	return Trial{ID: "fr_v1/c01-s6#" + string(rune('0'+n)), Suite: SuiteFR, Case: c, N: n}
}

func noBudget(env *Env) *Budget { return NewBudget(u.DB, env.Firm().ID, 0, env.started) }

// A trial runs the complete agent outcome and collects it: the delivered,
// verified answer, the dispatches, the usage from llm_calls and
// the model the provider said it ran, then judges it against its oracle.
func TestHarnessRunsTheCompleteOutcome(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-6-luna-pro-20261001", true)
	env := bootHarness(t, g, "")
	res := env.RunTrial(context.Background(), greetingTrial(t, 1), noBudget(env), time.Now().Add(2*time.Minute))
	if res.Status != StatusPassed {
		t.Fatalf("status %s, failures %v, model issues %v, error %q", res.Status, res.Failures, res.ModelIssues, res.Error)
	}
	o := res.Outcome
	if o.Outcome != types.AgentOutcomeCompleted || o.Dispatches != 2 || o.ControllerCalls != 2 || o.Answer.CompletedEvents != 1 {
		t.Fatalf("outcome %+v", o)
	}
	if !strings.Contains(o.Answer.Text, "Bonjour Maître") || o.Verification == nil || o.Verification.Result == types.VerificationBlocked {
		t.Fatalf("the verified answer was not delivered: %q %+v", o.Answer.Text, o.Verification)
	}
	if res.Usage.Succeeded != 2 || res.Usage.UnknownUsage != 0 || res.Usage.CostUSD < 2*fakeCost-1e-9 || res.Usage.CachedTokens != 1200 {
		t.Fatalf("usage %+v", res.Usage.PurposeUsage)
	}
	if res.ReturnedModels["openai/gpt-6-luna-pro"]["openai/gpt-6-luna-pro-20261001"] != 2 || res.ProxyCalls != 2 {
		t.Fatalf("returned models %v (proxy calls %d)", res.ReturnedModels, res.ProxyCalls)
	}
	if res.OutputSHA256 == "" || len(res.PromptSHA256) != 1 {
		t.Fatalf("output %q prompts %v", res.OutputSHA256, res.PromptSHA256)
	}
}

// The CPH cases pick the reviewed built-in compétence, which every firm is
// seeded with: the runner checks it is there and publishes nothing of its own.
func TestHarnessPicksTheBuiltinCompetence(t *testing.T) {
	env := bootHarness(t, newFakeGateway(t, "openai/gpt-6-luna-pro", true), "")
	_, fr, _ := loadAll(t)
	c, ok := fr.Case("c09-s2")
	if !ok || c.Competence != "requete-cph-resiliation-judiciaire" {
		t.Fatalf("c09-s2 does not pick the built-in CPH compétence: %q", c.Competence)
	}
	if err := env.ensureCompetence(c.Competence); err != nil {
		t.Fatal(err)
	}
	var n int64
	u.DB.Table("skills").Where("tenant_id = ? AND slug LIKE ?", env.Firm().ID, "requete-cph%").Count(&n)
	if n != 1 {
		t.Fatalf("%d CPH compétences for the evaluation firm, want the built-in alone", n)
	}
}

// A provider that answers with a model the release did not approve fails
// the trial, whatever the answer looks like.
func TestHarnessFailsAnUnapprovedModel(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-5-mini", true)
	env := bootHarness(t, g, "")
	res := env.RunTrial(context.Background(), greetingTrial(t, 1), noBudget(env), time.Now().Add(2*time.Minute))
	if res.Status != StatusFailed || len(res.ModelIssues) == 0 || !strings.Contains(res.ModelIssues[0], "unapproved substitution") {
		t.Fatalf("status %s, model issues %v", res.Status, res.ModelIssues)
	}
}

// Usage the provider never reported stays unknown and keeps the trial — and
// the report — from passing; nothing is priced at zero.
func TestHarnessUnknownUsageBlocksAPass(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-6-luna-pro", false)
	env := bootHarness(t, g, "")
	rep := &Report{Version: ReportVersion, Tier: TierSmoke}
	env.RunAll(context.Background(), []Trial{greetingTrial(t, 1)}, noBudget(env), time.Now().Add(2*time.Minute), rep)
	res := rep.Trials[0]
	if res.Status != StatusUnknownUsage || res.Usage.UnknownUsage != 2 || len(res.Failures) > 0 {
		t.Fatalf("status %s unknown %d failures %v", res.Status, res.Usage.UnknownUsage, res.Failures)
	}
	if rep.Status != StatusUnknownUsage {
		t.Fatalf("report status %s, want unknown_usage", rep.Status)
	}
}

// The budget is a stopping limit: once the known spend reaches it the
// running trial is cancelled as incomplete_budget, the suite stops, and the
// trials that did not run stay in the denominator.
func TestHarnessStopsAtTheBudget(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-6-luna-pro", true)
	report := t.TempDir() + "/report.json"
	env := bootHarness(t, g, report)
	// One greeting costs two dispatches: the first trial fits, the second
	// crosses the limit on its first dispatch.
	limit := 2.5 * fakeCost
	budget := NewBudget(u.DB, env.Firm().ID, limit, env.started)
	rep := &Report{Version: ReportVersion, Tier: TierSmoke, BudgetUSD: limit}
	trials := []Trial{greetingTrial(t, 1), greetingTrial(t, 2), greetingTrial(t, 3)}
	env.RunAll(context.Background(), trials, budget, time.Now().Add(5*time.Minute), rep)
	if rep.Status != StatusIncompleteBudget || rep.StoppedBy != StatusIncompleteBudget {
		t.Fatalf("report status %s (stopped by %q), want incomplete_budget", rep.Status, rep.StoppedBy)
	}
	// The second trial is cancelled mid-run when the watchdog sees the limit
	// first, or finishes when the run is faster than the poll; either way the
	// third never runs and still counts.
	if c := rep.Counts; c.Planned != 3 || c.NotRun != 1 || c.Passed+c.IncompleteBudget != 2 || c.Passed < 1 {
		t.Fatalf("counts %+v: the denominator must stay 3", rep.Counts)
	}
	if rep.Trials[0].Status != StatusPassed || rep.Usage.CostUSD < limit {
		t.Fatalf("first trial %s, spent %.4f", rep.Trials[0].Status, rep.Usage.CostUSD)
	}
	for _, f := range []string{"answer.md", "bound-output.json", "outcome.json"} {
		if _, err := os.Stat(rep.Trials[0].OutputDir + "/" + f); err != nil {
			t.Fatalf("the reviewable outputs were not written: %v", err)
		}
	}
	if !strings.HasPrefix(rep.Trials[0].OutputDir, OutputsDir(report)) {
		t.Fatalf("outputs at %s, want under %s", rep.Trials[0].OutputDir, OutputsDir(report))
	}
}

// Scripted provider faults reach the real runtime: the same failing call at
// every decision ends the turn in no_progress, and the dispatches are
// recorded as scripted, never as provider calls.
func TestHarnessScriptedFaults(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-6-luna-pro", true)
	env := bootHarness(t, g, "")
	_, fr, _ := loadAll(t)
	base, _ := fr.Case("c03-s6")
	base.ID = "harness-repeated"
	base.Faults = []Fault{{From: 1, Kind: FaultScriptedCall, Tool: types.ToolReadPiece,
		Arguments: json.RawMessage(`{"progress_note":"Je relis la pièce.","piece_id":"piece-inexistante","cursor":null,"max_chars":null}`)}}
	base.Oracle = Oracle{Outcomes: []string{types.AgentOutcomeNoProgress}, MaxDispatches: 5}
	res := env.RunTrial(context.Background(), Trial{ID: "harness/repeated#1", Suite: SuiteAgentFailures, Case: base, N: 1},
		noBudget(env), time.Now().Add(2*time.Minute))
	if res.Status != StatusPassed {
		t.Fatalf("status %s, failures %v, error %q", res.Status, res.Failures, res.Error)
	}
	if res.ScriptedCalls != res.Outcome.Dispatches || res.Outcome.ErrorRows == 0 || g.hits != 0 {
		t.Fatalf("scripted %d, dispatches %d, error rows %d, provider hits %d", res.ScriptedCalls, res.Outcome.Dispatches, res.Outcome.ErrorRows, g.hits)
	}
}

// The scripted interruptions of a09 (restart) and a10 (cancel) land between
// provider calls: the dispatch counter is reached while that dispatch's call
// is in flight, and cutting it would leave its spend unknown — a trial that
// can never pass. Each completion here takes a second, so the counter is
// always seen mid-call.
func TestHarnessInterruptionsNeverCutAProviderCall(t *testing.T) {
	g := newFakeGateway(t, "openai/gpt-6-luna-pro", true)
	g.delay = time.Second
	env := bootHarness(t, g, "")
	prevLease, prevBeat := runner.DefaultLease, runner.HeartbeatInterval
	runner.DefaultLease, runner.HeartbeatInterval = 5*time.Second, time.Second
	t.Cleanup(func() { runner.DefaultLease, runner.HeartbeatInterval = prevLease, prevBeat })
	_, fr, ag := loadAll(t)
	base, _ := fr.Case("c01-s6")
	for _, tc := range []struct {
		from    string
		kind    string
		outcome string
	}{
		{"a09-restart", ActionRestart, types.AgentOutcomeCompleted},
		{"a10-cancellation", ActionCancel, types.AgentOutcomeCancelled},
	} {
		shipped, ok := ag.Case(tc.from)
		if !ok || shipped.Action == nil || shipped.Action.Kind != tc.kind {
			t.Fatalf("%s no longer scripts a %s", tc.from, tc.kind)
		}
		c := base
		c.ID = "harness-" + tc.kind
		c.Action = &Action{Kind: tc.kind, AfterDispatch: 1}
		c.Oracle = Oracle{Outcomes: []string{tc.outcome}, MaxDispatches: 8}
		res := env.RunTrial(context.Background(), Trial{ID: "harness/" + tc.kind + "#1", Suite: SuiteAgentFailures, Case: c, N: 1},
			noBudget(env), time.Now().Add(2*time.Minute))
		if res.Status != StatusPassed || res.Usage.UnknownUsage != 0 {
			t.Fatalf("%s: status %s, unknown usage %d, failures %v, error %q", tc.kind, res.Status, res.Usage.UnknownUsage, res.Failures, res.Error)
		}
	}
	if env.proxy.InFlight() != 0 {
		t.Fatal("requests still in flight at the proxy")
	}
}

// The evaluation reads no .env file and never runs on the developer's
// database: a developer API polling it would claim the run's steps.
func TestBootRefusesTheDeveloperDatabase(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{
		"postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable",
		"host=localhost port=5433 user=nomos dbname=nomos sslmode=disable",
	} {
		t.Setenv("DATABASE_URL", dsn)
		if _, err := Boot(context.Background(), Options{Release: testRelease()}, m); err == nil || !strings.Contains(err.Error(), "developer") {
			t.Fatalf("%s: Boot = %v, want a refusal of the developer database", dsn, err)
		}
	}
	t.Setenv("DATABASE_URL", "")
	if _, err := Boot(context.Background(), Options{Release: testRelease()}, m); err == nil || !strings.Contains(err.Error(), "not exported") {
		t.Fatalf("Boot without DATABASE_URL = %v", err)
	}
	if got := databaseName("postgres://nomos:nomos@localhost:55440/nomos_eval?sslmode=disable"); got != "nomos_eval" {
		t.Fatalf("databaseName = %q", got)
	}
}

// The lawyer approves an output hash: what each [n] points to — source,
// identity, locator, quote — is in the review file and bound by that hash.
func TestReviewOutputsBindTheCitations(t *testing.T) {
	env := &Env{opts: Options{ReportPath: t.TempDir() + "/report.json"}}
	decided := time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC).Unix()
	ci := types.AgentCitation{Number: 1, SourceType: types.CitationSourceJudilibre, Court: "Cour de cassation",
		Chamber: "Chambre sociale", DecisionDate: &decided, ECLI: "ECLI:FR:CCASS:2024:SO00601", DecisionID: "665f0a"}
	outcome := func(identity string) Outcome {
		return Outcome{Outcome: types.AgentOutcomeCompleted,
			Answer: DeliveredAnswer{Text: "Le refus d'une modification du contrat n'est pas une faute [1]."},
			Citations: []CitationFacts{{Number: 1, SourceType: ci.SourceType, Label: "Cass. soc., 5 juin 2024", OnAnswer: true,
				Identity: identity, Quote: "le refus par le salarié d'une modification de son contrat de travail ne constitue pas une faute",
				Locator: &types.SourceLocator{Kind: types.LocatorDecision, Start: 120, End: 220},
				Checks:  map[string]string{types.CitationCheckOfficialMatched: types.CitationCheckPassed}}}}
	}
	o := outcome(citationIdentity(ci))
	res := &TrialResult{ID: "fr_v1/c07-s1#1", Outcome: &o}
	if err := env.writeOutputs(res, nil, o); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(res.OutputDir + "/answer.md")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := os.ReadFile(res.OutputDir + "/bound-output.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cass. soc., 5 juin 2024", "ECLI:FR:CCASS:2024:SO00601", "2024-06-05", "ne constitue pas une faute", "caractères 120–220"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("answer.md does not show %q:\n%s", want, md)
		}
	}
	for _, want := range []string{"ECLI:FR:CCASS:2024:SO00601", "ne constitue pas une faute"} {
		if !strings.Contains(string(bound), want) {
			t.Fatalf("bound-output.json does not bind %q", want)
		}
	}
	// Another decision behind the same [1] is another output.
	other := outcome("Cour de cassation · Chambre sociale · ECLI:FR:CCASS:2019:SO00001")
	again := &TrialResult{ID: res.ID, Outcome: &other}
	if err := env.writeOutputs(again, nil, other); err != nil {
		t.Fatal(err)
	}
	if again.OutputSHA256 == res.OutputSHA256 {
		t.Fatal("the output hash does not bind what the citation points to")
	}
}
