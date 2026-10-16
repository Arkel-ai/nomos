package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// These tests run against a REAL Postgres, because everything worth testing
// here is SQL: FOR UPDATE SKIP LOCKED, the conditional lease transition, the
// Postgres clock. An in-memory fake would test the mock, not the queue.

const defaultTestDSN = "postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"

var (
	testDB     *gorm.DB
	testTenant = "test-tenant-" + uuid.NewString()[:8]
)

// Per-run coordination for the fake steps.
var scoreboard = struct {
	sync.Mutex
	order map[string][]string      // run id -> step keys, in completion order
	hang  map[string]chan struct{} // run id -> released when the test says so
	runs  map[string]int           // run id -> how many times the hanging step ran
}{
	order: map[string][]string{},
	hang:  map[string]chan struct{}{},
	runs:  map[string]int{},
}

func record(runID, key string) {
	scoreboard.Lock()
	defer scoreboard.Unlock()
	scoreboard.order[runID] = append(scoreboard.order[runID], key)
}

func orderOf(runID string) []string {
	scoreboard.Lock()
	defer scoreboard.Unlock()
	return append([]string(nil), scoreboard.order[runID]...)
}

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner tests need Postgres at %s: %v\n", dsn, err)
		os.Exit(1)
	}
	if err := db.AutoMigrate(types.NomosModels()...); err != nil {
		fmt.Fprintf(os.Stderr, "AutoMigrate failed: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	u.DB = db

	// Compress the clock. The lease and the heartbeat stay long so no test
	// races an automatic renewal: lease expiry is forced explicitly, which
	// is the only way to assert the reaper's behaviour deterministically.
	PollInterval = 40 * time.Millisecond
	DefaultLease = 60 * time.Second
	HeartbeatInterval = 60 * time.Second
	ReaperInterval = time.Hour
	BaseBackoff = 40 * time.Millisecond

	Register("test_echo", echoStep, RegisterOptions{Pool: types.StepKindDB})
	Register("test_hang", hangStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	Register("test_block", blockStep, RegisterOptions{Pool: types.StepKindDB})
	Register("test_expand", expandStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 1})
	Register("test_commit", commitStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	Register("test_terminal", terminalStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 1})
	Register("test_fence", fenceStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 1})
	Register("test_fail", failStep, RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 1})
	OnRunTerminal(testHookWorkflow, func(_ *gorm.DB, run types.Run) error {
		record(run.ID, "hook:"+run.State+":"+run.FailureCode)
		return nil
	})

	code := m.Run()
	cleanup(db)
	os.Exit(code)
}

func cleanup(db *gorm.DB) {
	db.Exec(`DELETE FROM run_events WHERE tenant_id LIKE 'test-tenant-%'`)
	db.Exec(`DELETE FROM run_questions WHERE tenant_id LIKE 'test-tenant-%'`)
	db.Exec(`DELETE FROM step_attempts WHERE tenant_id LIKE 'test-tenant-%'`)
	db.Exec(`DELETE FROM run_step_deps WHERE run_id IN (SELECT id FROM runs WHERE tenant_id LIKE 'test-tenant-%')`)
	db.Exec(`DELETE FROM llm_calls WHERE tenant_id LIKE 'test-tenant-%'`)
	db.Exec(`DELETE FROM run_steps WHERE tenant_id LIKE 'test-tenant-%'`)
	db.Exec(`DELETE FROM runs WHERE tenant_id LIKE 'test-tenant-%'`)
}

// echoStep is a trivial db-only step: it records that it ran and succeeds.
func echoStep(_ context.Context, sc *StepCtx) (any, error) {
	record(sc.Run.ID, sc.Step.StepKey)
	return map[string]any{"step_key": sc.Step.StepKey, "attempt": sc.Step.AttemptCount}, nil
}

// expandStep grows the plan with one child depending on itself, keyed by its
// input — which lets a test aim the child at a key the run already has.
func expandStep(_ context.Context, sc *StepCtx) (any, error) {
	var in struct {
		ChildKey string `json:"child_key"`
	}
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	record(sc.Run.ID, sc.Step.StepKey)
	return Expansion{
		Output: map[string]any{"expanded": in.ChildKey},
		Steps: []StepSpec{{
			Key: in.ChildKey, Kind: "test_echo", Label: "child", DependsOn: []string{sc.Step.StepKey},
		}},
	}, nil
}

// testHookWorkflow is the workflow type whose runs report their terminal hook
// into the scoreboard.
const testHookWorkflow = "test_hook_wf"

// commitStep grows the plan with a child and writes a marker event in its
// Commit — failing that commit on the first attempt when asked to.
func commitStep(_ context.Context, sc *StepCtx) (any, error) {
	var in struct {
		FailFirst bool `json:"fail_first"`
	}
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	attempt := sc.Step.AttemptCount
	return Expansion{
		Output: map[string]any{"attempt": attempt},
		Steps:  []StepSpec{{Key: "child", Kind: "test_echo", Label: "child", DependsOn: []string{sc.Step.StepKey}}},
		Commit: func(tx *gorm.DB) error {
			if err := emitEvent(tx, &sc.Run, nil, "test.committed", map[string]any{"attempt": attempt}); err != nil {
				return err
			}
			if in.FailFirst && attempt == 1 {
				return errors.New("commit refused on the first attempt")
			}
			return nil
		},
	}, nil
}

// terminalStep ends its run with a stated code.
func terminalStep(_ context.Context, sc *StepCtx) (any, error) {
	return Terminal{
		Output:      map[string]any{"ended": true},
		FailureCode: "test_limit",
		Commit: func(tx *gorm.DB) error {
			return emitEvent(tx, &sc.Run, nil, "test.terminal_commit", nil)
		},
	}, nil
}

// fenceStep waits for the test, then tries one fenced write and records the
// outcome.
func fenceStep(ctx context.Context, sc *StepCtx) (any, error) {
	scoreboard.Lock()
	ch := scoreboard.hang[sc.Run.ID]
	scoreboard.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-time.After(20 * time.Second):
	}
	err := sc.WithLeaseTransaction(context.Background(), func(tx *gorm.DB) error {
		return emitEvent(tx, &sc.Run, nil, "test.fenced_write", nil)
	})
	switch {
	case err == nil:
		record(sc.Run.ID, "fence:ok")
	case errors.Is(err, ErrRunCancelled):
		record(sc.Run.ID, "fence:cancelled")
	case errors.Is(err, ErrLeaseLost):
		record(sc.Run.ID, "fence:lease_lost")
	default:
		record(sc.Run.ID, "fence:"+err.Error())
	}
	return nil, nil
}

func failStep(context.Context, *StepCtx) (any, error) {
	return nil, Permanent("test_broken", "deterministic failure")
}

func countEvents(t *testing.T, runID, evType string) int64 {
	t.Helper()
	var n int64
	testDB.Model(&types.RunEvent{}).Where("run_id = ? AND type = ?", runID, evType).Count(&n)
	return n
}

// hangStep blocks on its first attempt (simulating a worker that vanished
// mid-flight) and succeeds on every later one.
func hangStep(ctx context.Context, sc *StepCtx) (any, error) {
	scoreboard.Lock()
	scoreboard.runs[sc.Run.ID]++
	ch := scoreboard.hang[sc.Run.ID]
	scoreboard.Unlock()

	if sc.Step.AttemptCount == 1 {
		select {
		case <-ch:
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
		}
		record(sc.Run.ID, sc.Step.StepKey+"#1")
		return map[string]any{"attempt": 1}, nil
	}
	record(sc.Run.ID, sc.Step.StepKey+"#2")
	return map[string]any{"attempt": sc.Step.AttemptCount}, nil
}

// blockStep waits until the run is cancelled (or the test releases it), which
// is what a long external call looks like from the runner's point of view.
func blockStep(ctx context.Context, sc *StepCtx) (any, error) {
	record(sc.Run.ID, sc.Step.StepKey+":started")
	scoreboard.Lock()
	ch := scoreboard.hang[sc.Run.ID]
	scoreboard.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-time.After(20 * time.Second):
	}
	return map[string]any{"ok": true}, nil
}

func startRunner(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	Start(ctx, config.Config{
		StepConcurrencyOCR:    1,
		StepConcurrencyRender: 1,
		StepConcurrencyLLM:    2,
		StepConcurrencyDB:     8,
	})
	return cancel
}

func newHang(runID string) chan struct{} {
	ch := make(chan struct{})
	scoreboard.Lock()
	scoreboard.hang[runID] = ch
	scoreboard.Unlock()
	return ch
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stepByKey(t *testing.T, runID, key string) types.RunStep {
	t.Helper()
	var s types.RunStep
	if err := testDB.Where("run_id = ? AND step_key = ?", runID, key).Take(&s).Error; err != nil {
		t.Fatalf("step %q not found: %v", key, err)
	}
	return s
}

func runState(t *testing.T, runID string) string {
	t.Helper()
	var r types.Run
	if err := testDB.Where("id = ?", runID).Take(&r).Error; err != nil {
		t.Fatalf("run not found: %v", err)
	}
	return r.State
}

// ---------------------------------------------------------------------------

// A two-step run with one dependency executes IN ORDER and both steps land
// done. This is the whole point of the dependency predicate in the claim: the
// dependent must not be claimable while its parent is unfinished.
func TestDependencyOrdering(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID:     testTenant,
		DossierID:    "dossier-" + uuid.NewString()[:8],
		RequestedBy:  "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "two steps, in order",
		Steps: []StepSpec{
			{Key: "first", Kind: "test_echo", Label: "first"},
			{Key: "second", Kind: "test_echo", Label: "second", DependsOn: []string{"first"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, "the run to finish", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})

	if got := runState(t, run.ID); got != types.RunStateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got)
	}
	for _, key := range []string{"first", "second"} {
		if s := stepByKey(t, run.ID, key); s.State != types.StepStateSucceeded {
			t.Fatalf("step %q state = %q, want succeeded", key, s.State)
		}
	}
	order := orderOf(run.ID)
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("execution order = %v, want [first second]", order)
	}
	t.Logf("order=%v run=%s", order, run.ID)
}

// A step whose lease is force-expired mid-flight is requeued EXACTLY ONCE by
// the reaper: attempt_count becomes 2, there are two step_attempts rows (the
// abandoned one and the one that finished), and the stranded first attempt's
// output is discarded rather than published twice.
// The plan monitor draws itself from run.plan_ready alone, so the payload
// carries the steps and their edges — not a count.
func TestPlanReadyCarriesTheSteps(t *testing.T) {
	run, err := Enqueue(context.Background(), RunSpec{
		TenantID:     testTenant,
		DossierID:    "dossier-" + uuid.NewString()[:8],
		RequestedBy:  "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "a plan the monitor must be able to draw",
		Steps: []StepSpec{
			{Key: "root", Kind: "test_echo", Label: "root"},
			{Key: "leaf", Kind: "test_echo", Label: "leaf", DependsOn: []string{"root"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var payload string
	if err := testDB.Raw(`SELECT payload::text FROM run_events WHERE run_id = ? AND type = ? ORDER BY id LIMIT 1`,
		run.ID, types.EventPlanReady).Scan(&payload).Error; err != nil {
		t.Fatalf("plan_ready payload: %v", err)
	}
	var got struct {
		Steps []struct {
			ID        string   `json:"id"`
			StepKey   string   `json:"step_key"`
			Kind      string   `json:"kind"`
			Label     string   `json:"label"`
			State     string   `json:"state"`
			DependsOn []string `json:"depends_on"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("plan_ready payload is not a step list: %v (%s)", err, payload)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("plan_ready carries %d steps, want 2 (%s)", len(got.Steps), payload)
	}
	byKey := map[string]int{}
	for i, s := range got.Steps {
		if s.ID == "" || s.Kind == "" || s.State == "" {
			t.Fatalf("step %q is not renderable: %+v", s.StepKey, s)
		}
		byKey[s.StepKey] = i
	}
	leaf, ok := byKey["leaf"]
	if !ok {
		t.Fatalf("the plan is missing its leaf step (%s)", payload)
	}
	root, ok := byKey["root"]
	if !ok {
		t.Fatalf("the plan is missing its root step (%s)", payload)
	}
	if len(got.Steps[leaf].DependsOn) != 1 || got.Steps[leaf].DependsOn[0] != got.Steps[root].ID {
		t.Fatalf("leaf.depends_on = %v, want [%s] — the monitor cannot draw the graph without it",
			got.Steps[leaf].DependsOn, got.Steps[root].ID)
	}
	if len(got.Steps[root].DependsOn) != 0 {
		t.Fatalf("root.depends_on = %v, want empty", got.Steps[root].DependsOn)
	}
}

func TestLeaseExpiryRequeuesOnce(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	runID := ""
	run, err := Enqueue(context.Background(), RunSpec{
		TenantID:     testTenant,
		DossierID:    "dossier-" + uuid.NewString()[:8],
		RequestedBy:  "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "lose a lease",
		Steps:        []StepSpec{{Key: "flaky", Kind: "test_hang", Label: "flaky"}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	runID = run.ID
	release := newHang(runID)

	waitFor(t, "the step to be claimed", 10*time.Second, func() bool {
		return stepByKey(t, runID, "flaky").State == types.StepStateRunning
	})

	// Kill the lease the way a dead pod does: the row still says running,
	// but the deadline has passed on the POSTGRES clock.
	if err := testDB.Exec(`
		UPDATE run_steps SET lease_expires_at = now() - interval '1 second'
		 WHERE run_id = ? AND step_key = 'flaky'`, runID).Error; err != nil {
		t.Fatalf("forcing lease expiry: %v", err)
	}

	n, err := ReapExpired(context.Background(), testDB)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaper reclaimed %d leases, want 1", n)
	}
	// A second sweep must be a no-op: the step is no longer running with an
	// expired lease, so the conditional update matches nothing.
	if again, err := ReapExpired(context.Background(), testDB); err != nil || again != 0 {
		t.Fatalf("second sweep reclaimed %d (err=%v), want 0", again, err)
	}
	notifyPools()

	waitFor(t, "the second attempt to finish", 10*time.Second, func() bool {
		return stepByKey(t, runID, "flaky").State == types.StepStateSucceeded
	})

	step := stepByKey(t, runID, "flaky")
	if step.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", step.AttemptCount)
	}

	var attempts []types.StepAttempt
	if err := testDB.Where("run_step_id = ?", step.ID).Order("number").Find(&attempts).Error; err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("step_attempts rows = %d, want 2", len(attempts))
	}
	if attempts[0].State != types.AttemptStateAbandoned {
		t.Fatalf("attempt 1 state = %q, want abandoned", attempts[0].State)
	}
	if attempts[1].State != types.AttemptStateSucceeded {
		t.Fatalf("attempt 2 state = %q, want succeeded", attempts[1].State)
	}
	t.Logf("attempts: [1]=%s [2]=%s attempt_count=%d", attempts[0].State, attempts[1].State, step.AttemptCount)

	// Now let the stranded first attempt finish. Its publish is conditional
	// on (lease_owner, attempt_count), so it must be discarded — the step
	// stays on attempt 2's output and no third attempt row appears.
	close(release)
	time.Sleep(500 * time.Millisecond)

	step = stepByKey(t, runID, "flaky")
	if step.AttemptCount != 2 || step.State != types.StepStateSucceeded {
		t.Fatalf("after the stranded attempt returned: state=%q attempt_count=%d", step.State, step.AttemptCount)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(step.Output), &out); err != nil {
		t.Fatalf("decoding output: %v", err)
	}
	if got := fmt.Sprint(out["attempt"]); got != "2" {
		t.Fatalf("published output came from attempt %v, want 2", out["attempt"])
	}
}

// An ask_user step parks in waiting_input and its dependent does NOT run until
// answers are posted. No timer, no in-memory state: the pause is a row state.
func TestAskUserParksAndBlocksDependents(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID:     testTenant,
		DossierID:    "dossier-" + uuid.NewString()[:8],
		RequestedBy:  "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "ask before continuing",
		Steps: []StepSpec{
			{
				Key:   "clarify",
				Kind:  StepKindAskUser,
				Label: "Nature du contentieux",
				Input: AskUserInput{Questions: []Question{
					{Key: "nature", Prompt: "Nature du contentieux ?", Kind: types.QuestionKindSingle,
						Options: []QuestionOption{{Value: "prudhommes", Label: "Prud'hommes"}}},
					{Key: "position", Prompt: "Position procédurale ?", Kind: types.QuestionKindSingle},
				}},
			},
			{Key: "after", Kind: "test_echo", Label: "after", DependsOn: []string{"clarify"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, "the ask_user step to park", 10*time.Second, func() bool {
		return stepByKey(t, run.ID, "clarify").State == types.StepStateWaitingInput
	})
	if got := runState(t, run.ID); got != types.RunStateWaitingInput {
		t.Fatalf("run state = %q, want waiting_input", got)
	}

	// The dependent must stay queued while the question is open. Give the
	// claimer several poll intervals to get it wrong.
	time.Sleep(400 * time.Millisecond)
	if got := stepByKey(t, run.ID, "after").State; got != types.StepStateQueued {
		t.Fatalf("dependent state = %q while parked, want queued", got)
	}
	if order := orderOf(run.ID); len(order) != 0 {
		t.Fatalf("dependent ran before the answers: %v", order)
	}

	var questions []types.RunQuestion
	if err := testDB.Where("run_id = ?", run.ID).Order("question_key").Find(&questions).Error; err != nil {
		t.Fatalf("listing questions: %v", err)
	}
	if len(questions) != 2 {
		t.Fatalf("run_questions rows = %d, want 2", len(questions))
	}

	// A SKIP IS AN ANSWER: the second question is skipped explicitly, and
	// must land as a skipped row, not as silence.
	if _, err := RecordAnswers(context.Background(), testTenant, run.ID, "lawyer-1", []types.QuestionAnswer{
		{QuestionKey: "nature", Values: []string{"prudhommes"}},
		{QuestionKey: "position", Skipped: true},
	}); err != nil {
		t.Fatalf("RecordAnswers: %v", err)
	}

	waitFor(t, "the run to finish after the answers", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})
	if got := runState(t, run.ID); got != types.RunStateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got)
	}
	if order := orderOf(run.ID); len(order) != 1 || order[0] != "after" {
		t.Fatalf("execution order = %v, want [after]", order)
	}

	var skipped types.RunQuestion
	if err := testDB.Where("run_id = ? AND question_key = ?", run.ID, "position").
		Take(&skipped).Error; err != nil {
		t.Fatalf("skipped question missing: %v", err)
	}
	if skipped.State != types.QuestionStateSkipped {
		t.Fatalf("skipped question state = %q, want skipped", skipped.State)
	}
	if len(skipped.Answer) == 0 {
		t.Fatal("a skip must be recorded as an explicit answer, not as silence")
	}
	if skipped.AnsweredBy != "lawyer-1" {
		t.Fatalf("skipped question answered_by = %q", skipped.AnsweredBy)
	}
	t.Logf("skip recorded as answer: %s", string(skipped.Answer))
}

// A cancelled run stops being claimed, and cancellation NEVER erases the
// evidence: the step_attempts and llm_calls rows already written survive.
func TestCancelStopsClaimingAndKeepsEvidence(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID:     testTenant,
		DossierID:    "dossier-" + uuid.NewString()[:8],
		RequestedBy:  "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "cancel me",
		Steps: []StepSpec{
			{Key: "long", Kind: "test_block", Label: "long"},
			{Key: "later", Kind: "test_echo", Label: "later", DependsOn: []string{"long"}},
			{Key: "sibling", Kind: "test_echo", Label: "sibling", DependsOn: []string{"long"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	release := newHang(run.ID)
	defer close(release)

	waitFor(t, "the long step to start", 10*time.Second, func() bool {
		return stepByKey(t, run.ID, "long").State == types.StepStateRunning
	})

	// The evidence a cancellation must not touch: the attempt row the
	// claimer wrote, plus an llm_calls row keyed on its execution_id.
	step := stepByKey(t, run.ID, "long")
	var attempt types.StepAttempt
	if err := testDB.Where("run_step_id = ?", step.ID).Take(&attempt).Error; err != nil {
		t.Fatalf("attempt row missing: %v", err)
	}
	runID, stepID := run.ID, step.ID
	call := types.LLMCall{
		ID: uuid.NewString(), TenantID: testTenant,
		RunID: &runID, RunStepID: &stepID, StepAttemptID: &attempt.ID,
		ExecutionID: attempt.ExecutionID, Model: "google/gemini-3.7-flash",
		Purpose: types.LLMPurposeQualify, State: types.LLMCallStateSucceeded,
		PromptTokens: 1200, CompletionTokens: 300, UsageStatus: types.UsageStatusKnown,
	}
	if err := testDB.Create(&call).Error; err != nil {
		t.Fatalf("seeding llm_calls: %v", err)
	}

	if _, err := RequestCancel(context.Background(), testTenant, run.ID, "lawyer-1"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	if got := runState(t, run.ID); got != types.RunStateCancelled {
		t.Fatalf("run state = %q, want cancelled", got)
	}
	var r types.Run
	testDB.Where("id = ?", run.ID).Take(&r)
	if r.CancelRequestedAt == nil {
		t.Fatal("cancel_requested_at was not stamped — cancellation must be durable, not a CancelFunc")
	}

	// Queued dependents are cancelled, and the claimer must not pick
	// anything else up from this run.
	for _, key := range []string{"later", "sibling"} {
		if got := stepByKey(t, run.ID, key).State; got != types.StepStateCancelled {
			t.Fatalf("step %q state = %q, want cancelled", key, got)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if order := orderOf(run.ID); len(order) != 1 || order[0] != "long:started" {
		t.Fatalf("a cancelled run kept claiming: %v", order)
	}

	// Evidence survives.
	var attemptsAfter int64
	testDB.Model(&types.StepAttempt{}).Where("run_step_id = ?", step.ID).Count(&attemptsAfter)
	if attemptsAfter != 1 {
		t.Fatalf("step_attempts rows after cancel = %d, want 1", attemptsAfter)
	}
	var callsAfter int64
	testDB.Model(&types.LLMCall{}).Where("run_id = ?", run.ID).Count(&callsAfter)
	if callsAfter != 1 {
		t.Fatalf("llm_calls rows after cancel = %d, want 1", callsAfter)
	}
	t.Logf("after cancel: attempts=%d llm_calls=%d cancel_requested_at=%d",
		attemptsAfter, callsAfter, *r.CancelRequestedAt)
}

// A step that grows the plan commits its children WITH its success: the run
// never looks finished between the two, the child runs after its parent, and
// the monitor receives the whole plan.
func TestExpansionCommitsWithTheSuccess(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "expand",
		Steps: []StepSpec{{Key: "prepare", Kind: "test_expand", Label: "prepare",
			Input: map[string]any{"child_key": "child"}}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the run to finish", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})
	if got := runState(t, run.ID); got != types.RunStateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got)
	}
	if order := orderOf(run.ID); len(order) != 2 || order[0] != "prepare" || order[1] != "child" {
		t.Fatalf("execution order = %v, want [prepare child]", order)
	}
	var plans []types.RunEvent
	testDB.Where("run_id = ? AND type = ?", run.ID, types.EventPlanReady).Order("id").Find(&plans)
	if len(plans) != 2 {
		t.Fatalf("plan_ready events = %d, want the initial plan and the grown one", len(plans))
	}
	var grown struct {
		Steps []struct {
			StepKey string `json:"step_key"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(plans[1].Payload, &grown); err != nil || len(grown.Steps) != 2 {
		t.Fatalf("the grown plan does not carry both steps: %s (%v)", plans[1].Payload, err)
	}
}

// A child whose key the run already has is a defect in the step: it fails
// permanently, and the run carries that step's code, not a generic one.
func TestExpansionConflictFailsTheStep(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "expand onto an existing key",
		Steps: []StepSpec{
			{Key: "prepare", Kind: "test_expand", Label: "prepare", Input: map[string]any{"child_key": "taken"}},
			{Key: "taken", Kind: "test_echo", Label: "already planned", DependsOn: []string{"prepare"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the run to fail", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})
	if s := stepByKey(t, run.ID, "prepare"); s.State != types.StepStateFailed || s.ErrorCode != "expansion_conflict" {
		t.Fatalf("prepare = %s/%s, want failed/expansion_conflict", s.State, s.ErrorCode)
	}
	var stored types.Run
	testDB.Where("id = ?", run.ID).Take(&stored)
	if stored.State != types.RunStateFailed || stored.FailureCode != "expansion_conflict" {
		t.Fatalf("run = %s/%q, want failed/expansion_conflict", stored.State, stored.FailureCode)
	}
}

// An attempt that lost its lease publishes nothing — and adds nothing.
func TestExpansionOfALostLeaseAddsNothing(t *testing.T) {
	run, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "ghost attempt",
		Steps: []StepSpec{{Key: "prepare", Kind: "test_expand", Label: "prepare"}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	step := stepByKey(t, run.ID, "prepare")
	// The step was never claimed by "ghost": the conditional transition misses.
	finishSuccess(testDB, &StepCtx{Run: run, Step: step, WorkerID: "ghost"},
		Expansion{Output: map[string]any{}, Steps: []StepSpec{{Key: "child", Kind: "test_echo"}}})

	var children int64
	testDB.Model(&types.RunStep{}).Where("run_id = ? AND step_key = ?", run.ID, "child").Count(&children)
	if children != 0 {
		t.Fatalf("a lost lease still added %d step(s)", children)
	}
	if s := stepByKey(t, run.ID, "prepare"); s.State != types.StepStateQueued {
		t.Fatalf("prepare = %s, want untouched (queued)", s.State)
	}
}

// A step's Commit is part of its success: a failing commit rolls back the
// output, the children and the commit's own rows, and the retry commits all
// of them exactly once.
func TestExpansionCommitIsAtomicWithTheSuccess(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "commit with the success",
		Steps: []StepSpec{{Key: "accept", Kind: "test_commit", Label: "accept", Input: map[string]any{"fail_first": true}}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the run to finish", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})
	if got := runState(t, run.ID); got != types.RunStateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got)
	}
	if s := stepByKey(t, run.ID, "accept"); s.AttemptCount != 2 {
		t.Fatalf("accept attempts = %d, want 2 (the first commit was refused)", s.AttemptCount)
	}
	if n := countEvents(t, run.ID, "test.committed"); n != 1 {
		t.Fatalf("committed markers = %d, want exactly the second attempt's", n)
	}
	var children int64
	testDB.Model(&types.RunStep{}).Where("run_id = ? AND step_key = ?", run.ID, "child").Count(&children)
	if children != 1 || stepByKey(t, run.ID, "child").State != types.StepStateSucceeded {
		t.Fatalf("children = %d, want one, run once", children)
	}
}

// A Terminal step closes its run in the stated failure — not "succeeded"
// because nothing else was left — cancels what was still queued, and runs
// the workflow's terminal hook once, in the same commit.
func TestTerminalClosesTheRunWithItsCode(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	run, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: testHookWorkflow, Instruction: "end with a code",
		Steps: []StepSpec{
			{Key: "end", Kind: "test_terminal", Label: "end"},
			{Key: "after", Kind: "test_echo", Label: "after", DependsOn: []string{"end"}},
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the run to end", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, run.ID))
	})
	var stored types.Run
	testDB.Where("id = ?", run.ID).Take(&stored)
	if stored.State != types.RunStateFailed || stored.FailureCode != "test_limit" || stored.FinishedAt == nil {
		t.Fatalf("run = %s/%q, want failed/test_limit", stored.State, stored.FailureCode)
	}
	if s := stepByKey(t, run.ID, "end"); s.State != types.StepStateSucceeded {
		t.Fatalf("end = %s, want succeeded (its work was done)", s.State)
	}
	if s := stepByKey(t, run.ID, "after"); s.State != types.StepStateCancelled {
		t.Fatalf("after = %s, want cancelled", s.State)
	}
	if n := countEvents(t, run.ID, "test.terminal_commit"); n != 1 {
		t.Fatalf("terminal commits = %d, want 1", n)
	}
	time.Sleep(200 * time.Millisecond)
	if order := orderOf(run.ID); len(order) != 1 || order[0] != "hook:failed:test_limit" {
		t.Fatalf("scoreboard = %v, want the hook once and no further step", order)
	}
}

// The terminal hook also runs when a run ends any other way: a step that
// fails for good, or a cancellation.
func TestTerminalHookRunsOnFailureAndCancellation(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	failed, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: testHookWorkflow, Instruction: "break",
		Steps: []StepSpec{{Key: "broken", Kind: "test_fail", Label: "broken"}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the run to fail", 10*time.Second, func() bool {
		return types.TerminalRunState(runState(t, failed.ID))
	})
	if order := orderOf(failed.ID); len(order) != 1 || order[0] != "hook:failed:test_broken" {
		t.Fatalf("hook on failure = %v", order)
	}

	parked, err := Enqueue(context.Background(), RunSpec{
		TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
		WorkflowType: testHookWorkflow, Instruction: "wait then cancel",
		Steps: []StepSpec{{Key: "ask", Kind: StepKindAskUser, Label: "ask", Input: AskUserInput{
			Questions: []Question{{Key: "q", Prompt: "?", Kind: types.QuestionKindText, Required: true}},
		}}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, "the question to park", 10*time.Second, func() bool {
		return runState(t, parked.ID) == types.RunStateWaitingInput
	})
	var q types.RunQuestion
	testDB.Where("run_id = ?", parked.ID).Take(&q)
	if !q.Required {
		t.Fatal("a required question must be stored as required")
	}
	if _, err := RequestCancel(context.Background(), testTenant, parked.ID, "lawyer-1"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if order := orderOf(parked.ID); len(order) != 1 || order[0] != "hook:cancelled:cancelled_by_user" {
		t.Fatalf("hook on cancellation = %v", order)
	}
}

// A fenced write refuses once the run is cancelled, and once the lease is no
// longer this attempt's.
func TestWithLeaseTransactionFencesCancellationAndLeaseLoss(t *testing.T) {
	stop := startRunner(t)
	defer stop()

	start := func() string {
		run, err := Enqueue(context.Background(), RunSpec{
			TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
			WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "fenced write",
			Steps: []StepSpec{{Key: "fenced", Kind: "test_fence", Label: "fenced"}},
		})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return run.ID
	}

	cancelled := start()
	release := newHang(cancelled)
	waitFor(t, "the fenced step to run", 10*time.Second, func() bool {
		return stepByKey(t, cancelled, "fenced").State == types.StepStateRunning
	})
	if _, err := RequestCancel(context.Background(), testTenant, cancelled, "lawyer-1"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	close(release)
	waitFor(t, "the fenced write to be tried", 10*time.Second, func() bool { return len(orderOf(cancelled)) == 1 })
	if got := orderOf(cancelled)[0]; got != "fence:cancelled" {
		t.Fatalf("fenced write after cancellation = %s", got)
	}
	if n := countEvents(t, cancelled, "test.fenced_write"); n != 0 {
		t.Fatal("a fenced write committed on a cancelled run")
	}

	lost := start()
	release = newHang(lost)
	waitFor(t, "the fenced step to run", 10*time.Second, func() bool {
		return stepByKey(t, lost, "fenced").State == types.StepStateRunning
	})
	if err := testDB.Exec(`UPDATE run_steps SET lease_expires_at = now() - interval '1 second'
		WHERE run_id = ? AND step_key = 'fenced'`, lost).Error; err != nil {
		t.Fatalf("forcing lease expiry: %v", err)
	}
	close(release)
	waitFor(t, "the fenced write to be tried", 10*time.Second, func() bool { return len(orderOf(lost)) == 1 })
	if got := orderOf(lost)[0]; got != "fence:lease_lost" {
		t.Fatalf("fenced write after lease expiry = %s", got)
	}
}

// An expansion may only depend on steps that exist or come earlier in it,
// and may not grow the run past MaxRunSteps.
func TestExpansionGuards(t *testing.T) {
	enqueue := func(childKey string) types.Run {
		run, err := Enqueue(context.Background(), RunSpec{
			TenantID: testTenant, DossierID: "dossier-" + uuid.NewString()[:8], RequestedBy: "tester",
			WorkflowType: types.WorkflowDossierOrganizeV1, Instruction: "guarded expansion",
			Steps: []StepSpec{{Key: "prepare", Kind: "test_expand", Label: "prepare", Input: map[string]any{"child_key": childKey}}},
		})
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return run
	}
	claim := func(run types.Run) *StepCtx {
		step := stepByKey(t, run.ID, "prepare")
		if err := testDB.Exec(`UPDATE run_steps SET state = ?, lease_owner = ?, attempt_count = 1,
			lease_expires_at = now() + interval '1 minute' WHERE id = ?`,
			types.StepStateRunning, "guard-worker", step.ID).Error; err != nil {
			t.Fatalf("claiming: %v", err)
		}
		step = stepByKey(t, run.ID, "prepare")
		return &StepCtx{Run: run, Step: step, WorkerID: "guard-worker", TenantID: testTenant, db: testDB}
	}

	dangling := enqueue("child")
	finishSuccess(testDB, claim(dangling), Expansion{Output: map[string]any{}, Steps: []StepSpec{
		{Key: "child", Kind: "test_echo", DependsOn: []string{"nowhere"}},
	}})
	if s := stepByKey(t, dangling.ID, "prepare"); s.State != types.StepStateFailed || s.ErrorCode != "expansion_invalid" {
		t.Fatalf("dangling dependency: prepare = %s/%s, want failed/expansion_invalid", s.State, s.ErrorCode)
	}

	forward := enqueue("a")
	finishSuccess(testDB, claim(forward), Expansion{Output: map[string]any{}, Steps: []StepSpec{
		{Key: "a", Kind: "test_echo", DependsOn: []string{"b"}},
		{Key: "b", Kind: "test_echo", DependsOn: []string{"a"}},
	}})
	if s := stepByKey(t, forward.ID, "prepare"); s.ErrorCode != "expansion_invalid" {
		t.Fatalf("a cycle through a later child was accepted: %s/%s", s.State, s.ErrorCode)
	}

	prev := MaxRunSteps
	MaxRunSteps = 2
	defer func() { MaxRunSteps = prev }()
	big := enqueue("x")
	finishSuccess(testDB, claim(big), Expansion{Output: map[string]any{}, Steps: []StepSpec{
		{Key: "x", Kind: "test_echo"}, {Key: "y", Kind: "test_echo"},
	}})
	if s := stepByKey(t, big.ID, "prepare"); s.ErrorCode != "step_limit" {
		t.Fatalf("the step limit was not enforced: %s/%s", s.State, s.ErrorCode)
	}
}
