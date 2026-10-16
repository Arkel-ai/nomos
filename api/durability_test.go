package main

// THE DURABILITY SUITE (docs/PLAN.md §2, §4).
//
// §2 makes five promises about what happens when the pod dies mid-mission,
// and §4 describes the machinery that is supposed to keep them. Every one of
// them is a claim about behaviour under interruption, and a claim about
// behaviour under interruption that nobody interrupts is a comment.
//
// So this file kills things:
//
//   1. TestAtLeastOnceNotExactlyOnce — a step interrupted AFTER its external
//      call but BEFORE recording is retried; the accepted double-spend is
//      visible (two step_attempts, two llm_calls) and the published side
//      effect is NOT duplicated (§2.3, §4.3).
//   2. TestLeaseRecovery* — a force-expired lease is requeued exactly once,
//      even by eight reapers at the same time, and a slow-but-alive worker
//      that heartbeats is never double-claimed (§4.2).
//   3. TestClaimCommitsBeforeExternalCall — a second, independent database
//      connection can see the step as `running` WHILE it executes, which is
//      only true if the claim transaction committed first (§4.2).
//   4. TestIdempotentPublish* — a killed and replayed publish leaves exactly
//      one artifact row and one blob object (§4.3).
//   5. TestCostAccounting* — every gateway call leaves durable evidence, and
//      a missing usage trailer is recorded as `unknown`, never estimated
//      (§4.6).
//
// Everything here runs against the REAL Postgres from the dev stack, because
// everything here IS SQL: FOR UPDATE SKIP LOCKED, the conditional lease
// transition, the Postgres clock. A fake would test the fake.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// ---------------------------------------------------------------------------
// Fixtures: three step kinds that model the three ways work is interrupted
// ---------------------------------------------------------------------------

const (
	// kindSpend models a step whose external call is already paid for by the
	// time the pod dies: it records the call, publishes its idempotent side
	// effect, and only then returns. Attempt 1 never returns.
	kindSpend = "durability_spend"
	// kindProbe reads its own row back from an INDEPENDENT connection while
	// it is running. If the claim held a transaction open, that read would
	// not see `running`.
	kindProbe = "durability_probe"
	// kindHold blocks until the test releases it — a worker that is slow but
	// perfectly alive.
	kindHold = "durability_hold"
)

// board is the cross-goroutine scoreboard the fake steps report through.
var board = struct {
	sync.Mutex
	release map[string]chan struct{} // run id -> released when the test says so
	probe   map[string]probeResult   // run id -> what the second connection saw
	entered map[string]int           // run id -> how many attempts entered the step
}{
	release: map[string]chan struct{}{},
	probe:   map[string]probeResult{},
	entered: map[string]int{},
}

// probeResult is what an independent connection observed about a step that
// was, at that moment, mid-execution.
type probeResult struct {
	stepState   string
	leaseOwner  string
	attemptRows int64
	eventRows   int64
	inTx        bool
	err         error
}

func release(runID string) chan struct{} {
	ch := make(chan struct{})
	board.Lock()
	board.release[runID] = ch
	board.Unlock()
	return ch
}

func releaseChan(runID string) chan struct{} {
	board.Lock()
	defer board.Unlock()
	return board.release[runID]
}

func entered(runID string) int {
	board.Lock()
	defer board.Unlock()
	return board.entered[runID]
}

func init() {
	// Compress the clock so the suite runs in seconds. The lease and the
	// heartbeat stay LONG on purpose: expiry is always forced explicitly,
	// which is the only deterministic way to assert the reaper.
	runner.PollInterval = 40 * time.Millisecond
	runner.DefaultLease = 60 * time.Second
	runner.HeartbeatInterval = 60 * time.Second
	runner.ReaperInterval = time.Hour
	runner.BaseBackoff = 40 * time.Millisecond

	runner.Register(kindSpend, spendStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	runner.Register(kindProbe, probeStep, runner.RegisterOptions{Pool: types.StepKindDB})
	runner.Register(kindHold, holdStep, runner.RegisterOptions{Pool: types.StepKindDB})
}

// spendStep is the whole at-least-once story in one function.
//
// Order matters and mirrors production: the money is spent first, the
// idempotent side effect converges second, the result is recorded last. The
// window between step two and step three is the one §2.3 says we cannot
// close, so attempt 1 sits in it forever.
func spendStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	board.Lock()
	board.entered[sc.Run.ID]++
	board.Unlock()

	// (1) THE EXTERNAL CALL. Recorded with this attempt's execution_id, which
	// the claim minted before dispatch — so a double-spend is attributable
	// afterwards even though it is not preventable (§4.6).
	runID, stepID, attemptID := sc.Run.ID, sc.Step.ID, sc.Attempt.ID
	call := types.LLMCall{
		ID: uuid.NewString(), TenantID: sc.TenantID,
		RunID: &runID, RunStepID: &stepID, StepAttemptID: &attemptID,
		ExecutionID: sc.ExecutionID, Model: "test/fake-model",
		Purpose: types.LLMPurposeQualify, State: types.LLMCallStateSucceeded,
		PromptTokens: 1000, CompletionTokens: 100, UsageStatus: types.UsageStatusKnown,
	}
	if err := sc.DB().Create(&call).Error; err != nil {
		return nil, err
	}

	// (2) THE PUBLISHED SIDE EFFECT. The key is derived from the step's
	// STABLE idempotency_key, and the row insert is an upsert on the unique
	// s3_key — the two mechanisms §4.3 names. A replay overwrites; it does
	// not duplicate.
	key := sidecarKey(sc.TenantID, sc.DossierID, sc.Step.IdempotencyKey)
	if err := u.Blob.Put(ctx, key, strings.NewReader("published bytes"), 15, "application/octet-stream"); err != nil {
		return nil, err
	}
	asset := types.Asset{
		ID: uuid.NewString(), TenantID: sc.TenantID, S3Key: key,
		Kind: types.AssetKindArtifact, Mime: "application/octet-stream",
		Size: 15, SHA256: strings.Repeat("b", 64), CreatedBy: "durability-suite",
	}
	if err := sc.DB().Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "s3_key"}}, DoNothing: true,
	}).Create(&asset).Error; err != nil {
		return nil, err
	}

	// (3) RECORDING. Attempt 1 dies here: the pod is gone, the lease will
	// expire, and the reaper will hand the step to somebody else.
	if sc.Step.AttemptCount == 1 {
		select {
		case <-releaseChan(sc.Run.ID):
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
	return map[string]any{"attempt": sc.Step.AttemptCount, "key": key}, nil
}

// sidecarKey is the content-addressed key of spendStep's side effect. It
// depends only on the tenant, the dossier and the step's stable
// idempotency_key — never on the attempt — which is exactly what makes a
// replay converge (§4.3).
func sidecarKey(tenantID, dossierID, idempotencyKey string) string {
	return u.DossierKey(tenantID, dossierID, "sidecar-"+idempotencyKey+".bin")
}

// probeStep asks a SECOND, independent connection what it can see about this
// step while the step is still running.
func probeStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	res := probeResult{}
	db, closeDB, err := independentConn()
	if err != nil {
		res.err = err
	} else {
		defer closeDB()
		var step types.RunStep
		if err := db.WithContext(ctx).Where("id = ?", sc.Step.ID).Take(&step).Error; err != nil {
			res.err = err
		} else {
			res.stepState, res.leaseOwner = step.State, step.LeaseOwner
		}
		db.WithContext(ctx).Model(&types.StepAttempt{}).
			Where("run_step_id = ?", sc.Step.ID).Count(&res.attemptRows)
		db.WithContext(ctx).Model(&types.RunEvent{}).
			Where("run_step_id = ?", sc.Step.ID).Count(&res.eventRows)

		// Belt and braces: ask Postgres itself whether a backend is holding a
		// transaction open ON THE QUEUE while this step body runs. A claim
		// that did not commit first would sit here, idle in transaction, with
		// its run_steps statement as the last thing it ran.
		//
		// The predicate is narrowed to run_steps on purpose: every test
		// package in this module shares one Postgres, so counting *any*
		// idle-in-transaction backend makes this assertion report another
		// suite's unrelated connection as a claim-boundary violation.
		var idleInTx int64
		db.WithContext(ctx).Raw(
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND state = 'idle in transaction'
			    AND query ILIKE '%run_steps%'`).
			Scan(&idleInTx)
		res.inTx = idleInTx > 0
	}

	board.Lock()
	board.probe[sc.Run.ID] = res
	board.Unlock()
	return map[string]any{"probed": true}, nil
}

// holdStep is a worker that is slow but perfectly alive.
func holdStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	board.Lock()
	board.entered[sc.Run.ID]++
	board.Unlock()
	select {
	case <-releaseChan(sc.Run.ID):
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
	}
	return map[string]any{"attempt": sc.Step.AttemptCount}, nil
}

// independentConn opens a connection pool of its own, so a read through it
// cannot be served by the transaction the runner is using.
func independentConn() (*gorm.DB, func(), error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = testsupport.DefaultDSN
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, func() {}, err
	}
	return db, func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	}, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func startRunner(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx, config.Config{
		StepConcurrencyOCR:    1,
		StepConcurrencyRender: 1,
		StepConcurrencyLLM:    2,
		StepConcurrencyDB:     8,
	})
	t.Cleanup(cancel)
}

func enqueueOne(t *testing.T, firm testsupport.Firm, dossierID, key, kind string) types.Run {
	t.Helper()
	run, err := runner.Enqueue(context.Background(), runner.RunSpec{
		TenantID:     firm.ID,
		DossierID:    dossierID,
		RequestedBy:  firm.UserID,
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "durability suite",
		Steps:        []runner.StepSpec{{Key: key, Kind: kind, Label: key}},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return run
}

func stepOf(t *testing.T, runID, key string) types.RunStep {
	t.Helper()
	var s types.RunStep
	if err := u.DB.Where("run_id = ? AND step_key = ?", runID, key).Take(&s).Error; err != nil {
		t.Fatalf("step %q not found: %v", key, err)
	}
	return s
}

// expireLease kills a lease the way a dead pod does: the row still says
// running, but the deadline has passed on the POSTGRES clock. Pod clocks are
// never consulted (§9).
func expireLease(t *testing.T, stepID string) {
	t.Helper()
	if err := u.DB.Exec(
		`UPDATE run_steps SET lease_expires_at = now() - interval '1 second' WHERE id = ?`,
		stepID).Error; err != nil {
		t.Fatalf("forcing lease expiry: %v", err)
	}
}

// renewLease is what a heartbeat does: extend the deadline, same owner, same
// attempt. A worker that does this between the reaper's read and its write is
// the double-claim the conditional update exists to prevent (§4.2).
func renewLease(t *testing.T, stepID string) {
	t.Helper()
	if err := u.DB.Exec(
		`UPDATE run_steps SET heartbeat_at = now(),
		        lease_expires_at = now() + interval '120 seconds' WHERE id = ?`,
		stepID).Error; err != nil {
		t.Fatalf("renewing lease: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1. AT-LEAST-ONCE, NOT EXACTLY-ONCE (§2.3, §4.3)
// ---------------------------------------------------------------------------

// A step that is interrupted after its external call but before recording IS
// retried. That retry is a double-spend — the provider was paid twice — and
// §2.3 says so out loud rather than pretending otherwise. What the design owes
// us is not prevention but two things this test insists on:
//
//   - the double-spend is VISIBLE: two step_attempts rows with distinct
//     execution_ids, and two llm_calls rows carrying them. An invisible
//     double-spend is an unauditable one, and cost is a billing record;
//   - the PUBLISHED side effect converges: one object, one row, because the
//     key derives from the step's stable idempotency_key and the insert is an
//     upsert on a unique constraint.
func TestAtLeastOnceNotExactlyOnce(t *testing.T) {
	testsupport.Boot(t)
	blob := testsupport.NewMemBlob(t)
	firm := testsupport.NewFirm(t, "atleastonce")
	startRunner(t)

	dossierID := uuid.NewString()
	run := enqueueOne(t, firm, dossierID, "spend", kindSpend)
	rel := release(run.ID)

	testsupport.WaitFor(t, "the step to be claimed and spend", 10*time.Second, func() bool {
		return entered(run.ID) == 1 &&
			testsupport.Count(t, "llm_calls", "run_id = ?", run.ID) == 1
	})
	step := stepOf(t, run.ID, "spend")

	// The pod dies HERE: after the call, after the publish, before recording.
	expireLease(t, step.ID)
	n, err := runner.ReapExpired(context.Background(), u.DB)
	if err != nil || n != 1 {
		t.Fatalf("ReapExpired = (%d, %v), want (1, nil)", n, err)
	}

	testsupport.WaitFor(t, "the replay to finish", 15*time.Second, func() bool {
		return stepOf(t, run.ID, "spend").State == types.StepStateSucceeded
	})

	// --- the accepted double-spend must be visible ---------------------------
	var attempts []types.StepAttempt
	if err := u.DB.Where("run_step_id = ?", step.ID).Order("number").
		Find(&attempts).Error; err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("step_attempts rows = %d, want 2 — a retried step whose attempts are not both recorded is an unauditable double-spend", len(attempts))
	}
	if attempts[0].ExecutionID == attempts[1].ExecutionID {
		t.Fatal("both attempts share one execution_id — the double-spend is not attributable")
	}
	if attempts[0].State != types.AttemptStateAbandoned {
		t.Fatalf("attempt 1 state = %q, want abandoned", attempts[0].State)
	}

	var calls []types.LLMCall
	if err := u.DB.Where("run_id = ?", run.ID).Order("started_at").Find(&calls).Error; err != nil {
		t.Fatalf("listing llm_calls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("llm_calls rows = %d, want 2 — the second (paid, wasted) call must be on the record", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if seen[c.ExecutionID] {
			t.Fatalf("llm_calls execution_id %s is not unique", c.ExecutionID)
		}
		seen[c.ExecutionID] = true
		if c.RunStepID == nil || *c.RunStepID != step.ID {
			t.Fatalf("llm_call %s is not tied to the step that made it", c.ID)
		}
	}

	// --- the published side effect must NOT be duplicated --------------------
	key := sidecarKey(firm.ID, dossierID, step.IdempotencyKey)
	if got := testsupport.Count(t, "assets", "tenant_id = ? AND s3_key = ?", firm.ID, key); got != 1 {
		t.Fatalf("assets rows for the published key = %d, want exactly 1", got)
	}
	if got := testsupport.Count(t, "assets", "tenant_id = ?", firm.ID); got != 1 {
		t.Fatalf("the replay published %d assets, want exactly 1", got)
	}
	if keys := blob.Keys(""); len(keys) != 1 || keys[0] != key {
		t.Fatalf("blob objects = %v, want exactly [%s]", keys, key)
	}
	if p := blob.Puts(key); p != 2 {
		t.Fatalf("the key was written %d times, want 2 (both attempts converge on ONE key)", p)
	}

	// --- and the stranded attempt's result is discarded, not published -------
	close(rel)
	time.Sleep(500 * time.Millisecond)
	after := stepOf(t, run.ID, "spend")
	if after.AttemptCount != 2 || after.State != types.StepStateSucceeded {
		t.Fatalf("after the stranded attempt returned: state=%q attempt_count=%d", after.State, after.AttemptCount)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(after.Output), &out); err != nil {
		t.Fatalf("decoding output: %v", err)
	}
	if fmt.Sprint(out["attempt"]) != "2" {
		t.Fatalf("published output came from attempt %v, want 2", out["attempt"])
	}

	t.Logf("double-spend visible: attempts=%d llm_calls=%d | side effect converged: assets=1 blob_keys=1 puts=%d",
		len(attempts), len(calls), blob.Puts(key))
}

// ---------------------------------------------------------------------------
// 2. LEASE RECOVERY (§4.2)
// ---------------------------------------------------------------------------

// seedLeasedStep builds a real plan and then puts its single step into the
// state the claim transaction would have left it in: running, attempt 1, held
// by a worker that has since disappeared.
//
// The run is flagged cancel_requested_at so NO claimer anywhere can pick the
// step up after the reaper requeues it — claim.go excludes flagged runs. That
// is what makes "the reaper requeued it exactly once" assertable instead of
// racy: the reaper is the only actor in the test.
func seedLeasedStep(t *testing.T, firm testsupport.Firm, label, owner string) (types.Run, types.RunStep) {
	t.Helper()
	run := enqueueOne(t, firm, uuid.NewString(), label, kindHold)
	if err := u.DB.Exec(
		`UPDATE runs SET cancel_requested_at = EXTRACT(EPOCH FROM now())::bigint WHERE id = ?`,
		run.ID).Error; err != nil {
		t.Fatalf("freezing the run: %v", err)
	}
	step := stepOf(t, run.ID, label)
	if err := u.DB.Exec(`
		UPDATE run_steps
		   SET state = ?, attempt_count = 1, lease_owner = ?,
		       lease_expires_at = now() + interval '60 seconds', heartbeat_at = now()
		 WHERE id = ?`, types.StepStateRunning, owner, step.ID).Error; err != nil {
		t.Fatalf("seeding the lease: %v", err)
	}
	att := types.StepAttempt{
		ID: uuid.NewString(), TenantID: firm.ID, RunStepID: step.ID, Number: 1,
		ExecutionID: uuid.NewString(), WorkerID: owner, State: types.AttemptStateRunning,
		Retryable: true,
	}
	if err := u.DB.Create(&att).Error; err != nil {
		t.Fatalf("seeding the attempt row: %v", err)
	}
	return run, stepOf(t, run.ID, label)
}

// Eight reapers sweep the same expired lease at the same time. Exactly one may
// win. This is the multi-replica case: §2.2 says replicas may be > 1 with no
// worker/API role split, so "the reaper" is really N reapers racing.
func TestLeaseRecoveryRequeuesExactlyOnceUnderConcurrentReapers(t *testing.T) {
	testsupport.Boot(t)
	firm := testsupport.NewFirm(t, "reaprace")

	run, step := seedLeasedStep(t, firm, "held", "dead-pod-1")
	expireLease(t, step.ID)

	const reapers = 8
	var wg sync.WaitGroup
	results := make([]int, reapers)
	errs := make([]error, reapers)
	start := make(chan struct{})
	for i := 0; i < reapers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, closeDB, err := independentConn()
			if err != nil {
				errs[i] = err
				return
			}
			defer closeDB()
			<-start
			results[i], errs[i] = runner.ReapExpired(context.Background(), db)
		}(i)
	}
	close(start)
	wg.Wait()

	total := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("reaper %d failed: %v", i, errs[i])
		}
		total += results[i]
	}
	if total != 1 {
		t.Fatalf("%d reapers reclaimed the lease %d times, want exactly 1 — the conditional update on the old lease owner did not hold", reapers, total)
	}

	// One requeue means one new attempt, not eight.
	reclaimed := stepOf(t, run.ID, "held")
	if reclaimed.AttemptCount != 1 {
		t.Fatalf("attempt_count after the requeue = %d, want 1 (the reaper requeues; it does not re-claim)", reclaimed.AttemptCount)
	}
	if reclaimed.LeaseOwner != "" {
		t.Fatalf("lease_owner after the requeue = %q, want empty", reclaimed.LeaseOwner)
	}
	var abandoned int64
	u.DB.Model(&types.StepAttempt{}).
		Where("run_step_id = ? AND state = ?", step.ID, types.AttemptStateAbandoned).
		Count(&abandoned)
	if abandoned != 1 {
		t.Fatalf("abandoned attempt rows = %d, want 1", abandoned)
	}
	t.Logf("8 concurrent reapers, %d reclaim(s), %d abandoned attempt row(s)", total, abandoned)
}

// A worker that is SLOW BUT ALIVE heartbeats between the reaper's candidate
// read and its conditional write. Its row no longer matches the predicate, so
// it is never double-claimed — the single most important line in reaper.go.
func TestLeaseRecoverySlowButAliveWorkerIsNotDoubleClaimed(t *testing.T) {
	testsupport.Boot(t)
	firm := testsupport.NewFirm(t, "slowalive")

	run, step := seedLeasedStep(t, firm, "held", "slow-but-alive-pod")

	// The lease lapses… and then the worker's heartbeat lands. This is the
	// exact interleaving the conditional update is written for.
	expireLease(t, step.ID)
	renewLease(t, step.ID)

	n, err := runner.ReapExpired(context.Background(), u.DB)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if n != 0 {
		t.Fatalf("the reaper reclaimed %d live lease(s) — a heartbeating worker was double-claimed", n)
	}
	after := stepOf(t, run.ID, "held")
	if after.State != types.StepStateRunning || after.LeaseOwner != step.LeaseOwner {
		t.Fatalf("a live step was disturbed: state=%q owner=%q", after.State, after.LeaseOwner)
	}
	if got := testsupport.Count(t, "step_attempts", "run_step_id = ?", step.ID); got != 1 {
		t.Fatalf("step_attempts rows = %d, want 1 — a second attempt was opened for a live worker", got)
	}

	// And a stale reaper that saw the OLD lease owner must still miss after
	// the owner changed: the predicate keys on (state, lease_owner,
	// attempt_count, expiry), not on expiry alone.
	expireLease(t, step.ID)
	if err := u.DB.Exec(
		`UPDATE run_steps SET lease_owner = ?, attempt_count = attempt_count + 1 WHERE id = ?`,
		"some-other-pod", step.ID).Error; err != nil {
		t.Fatalf("simulating a takeover: %v", err)
	}
	before := testsupport.Count(t, "step_attempts", "run_step_id = ?", step.ID)
	if _, err := runner.ReapExpired(context.Background(), u.DB); err != nil {
		t.Fatalf("ReapExpired after takeover: %v", err)
	}
	if got := testsupport.Count(t, "step_attempts", "run_step_id = ?", step.ID); got != before {
		t.Fatalf("attempt rows changed from %d to %d after a takeover sweep", before, got)
	}
	t.Log("heartbeat wins the race; a stale owner string never reclaims")
}

// ---------------------------------------------------------------------------
// 3. NO TRANSACTION ACROSS AN EXTERNAL CALL (§4.2)
// ---------------------------------------------------------------------------

// "Never hold a row lock or transaction across OCR, S3, pandoc or an LLM
// call." A 3-minute request behind an open transaction consumes a connection,
// retains row versions, blocks vacuum and fails badly during failover.
//
// The proof is a read from a SECOND, INDEPENDENT connection taken while the
// step body is executing: it must already see the row as `running`, with the
// lease stamped, and the step_attempts and run_events rows the claim wrote in
// the SAME transaction. If the claim were still open, none of that would be
// visible and the row would very likely be lock-blocked.
func TestClaimCommitsBeforeExternalCall(t *testing.T) {
	testsupport.Boot(t)
	firm := testsupport.NewFirm(t, "txboundary")
	startRunner(t)

	run := enqueueOne(t, firm, uuid.NewString(), "probe", kindProbe)
	testsupport.WaitFor(t, "the probe step to finish", 15*time.Second, func() bool {
		return types.TerminalRunState(runStateOf(t, run.ID))
	})
	if got := runStateOf(t, run.ID); got != types.RunStateSucceeded {
		t.Fatalf("run state = %q, want succeeded", got)
	}

	board.Lock()
	res := board.probe[run.ID]
	board.Unlock()

	if res.err != nil {
		t.Fatalf("the independent connection could not read the step mid-flight: %v", res.err)
	}
	if res.stepState != types.StepStateRunning {
		t.Fatalf("a second connection saw state=%q while the step was executing, want running — the claim transaction had NOT committed", res.stepState)
	}
	if res.leaseOwner == "" {
		t.Fatal("the lease owner was not visible from another connection — the claim had not committed")
	}
	if res.attemptRows != 1 {
		t.Fatalf("step_attempts rows visible mid-flight = %d, want 1 — the attempt row must commit WITH the claim", res.attemptRows)
	}
	if res.eventRows < 1 {
		t.Fatalf("run_events rows visible mid-flight = %d, want ≥ 1 — step.started must commit WITH the claim", res.eventRows)
	}
	if res.inTx {
		t.Fatal("a backend was 'idle in transaction' while the step body ran — something is holding a transaction across the call")
	}
	t.Logf("mid-flight, from another connection: state=%s lease_owner=%s attempts=%d events=%d idle_in_tx=false",
		res.stepState, res.leaseOwner, res.attemptRows, res.eventRows)
}

func runStateOf(t *testing.T, runID string) string {
	t.Helper()
	var r types.Run
	if err := u.DB.Where("id = ?", runID).Take(&r).Error; err != nil {
		t.Fatalf("run not found: %v", err)
	}
	return r.State
}

// ---------------------------------------------------------------------------
// 5. COST ACCOUNTING COMPLETENESS (§4.6)
// ---------------------------------------------------------------------------

// Every gateway call must leave durable evidence even when it FAILS, because
// the failure mode §4.6 exists to fix is "the pod died on the expensive run
// and the cost record went with it".
//
// This test hits the real gateway with a model that does not exist. It skips
// (never fails) without OPENROUTER_API_KEY: a missing key is a missing
// environment, not a broken invariant.
func TestCostAccountingRecordsFailedCalls(t *testing.T) {
	testsupport.Boot(t)
	if !u.LLMConfigured() {
		t.Skip("OPENROUTER_API_KEY not set — the live cost-path test needs the real gateway")
	}
	firm := testsupport.NewFirm(t, "costfail")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := u.Chat(ctx, u.ChatOpts{
		Model:    "nomos/model-that-does-not-exist",
		Purpose:  types.LLMPurposeChat,
		User:     "ping",
		TenantID: firm.ID,
	})
	if err == nil {
		t.Fatal("expected the bogus model to fail")
	}
	if res.ExecutionID == "" {
		t.Fatal("no execution_id was minted for a failed call — §4.6(1) requires it BEFORE dispatch")
	}

	var call types.LLMCall
	if err := u.DB.Where("execution_id = ?", res.ExecutionID).Take(&call).Error; err != nil {
		t.Fatalf("a failed gateway call left NO llm_calls row (execution_id=%s): %v", res.ExecutionID, err)
	}
	if call.State != types.LLMCallStateFailed {
		t.Fatalf("llm_calls.state = %q, want failed", call.State)
	}
	if call.UsageStatus != types.UsageStatusUnknown {
		t.Fatalf("llm_calls.usage_status = %q, want unknown — a call with no usage trailer must NOT be estimated", call.UsageStatus)
	}
	if call.PromptTokens != 0 || call.CompletionTokens != 0 {
		t.Fatalf("a call with unknown usage carries tokens (%d/%d) — that is an estimate, and an estimate is not auditable billing",
			call.PromptTokens, call.CompletionTokens)
	}
	if call.TenantID != firm.ID {
		t.Fatalf("llm_calls.tenant_id = %q, want %q — cost is attributed per firm", call.TenantID, firm.ID)
	}

	// And DELIBERATELY no cost. A call that never returned usage has no
	// provider-reported cost; recording one would be billing a number nobody
	// measured, which §4.6 rules out.
	if call.CostNanoUSD != 0 {
		t.Fatalf("a FAILED call carries a cost (%d nano-USD) — there is no measured usage to bill", call.CostNanoUSD)
	}
	t.Logf("failed call recorded: execution_id=%s state=%s usage_status=%s tokens=%d/%d",
		call.ExecutionID, call.State, call.UsageStatus, call.PromptTokens, call.CompletionTokens)
}

// The two live tests above can only afford to exercise the entry points they
// pay for. This one covers the REST of §4.6's word "every": it reads
// api/utils and insists that every place a request leaves the process for the
// gateway sits inside a function that mints an execution id BEFORE dispatch
// and reconciles the row afterwards.
//
// It is a source guard rather than a runtime one because the failure it
// catches is a new gateway endpoint — a moderation call, a vision call, an
// image call — added by someone who copied the HTTP plumbing and not the
// bookkeeping. That call would never appear in a cost report, and nothing
// else in the suite would notice.
// gatewayCallSite is one function that sends a request to the gateway, and
// what bookkeeping that function does around it.
type gatewayCallSite struct {
	file, fn string
	begins   bool
	finishes int
}

// gatewayDispatch matches the transport helpers a call has to go through to
// reach OpenRouter. Adding a fourth one and not adding it here is the way this
// guard goes blind, so the count of sites it finds is asserted too.
var gatewayDispatch = regexp.MustCompile(`\bg\.post\(|\bg\.attempt\(|gatewayHandle\(\)\.post\(`)

// gatewayCallSitesIn parses one Go source and reports every top-level function
// that dispatches. Methods on the gateway itself are the plumbing every
// recorded caller goes through, not calls in their own right, so they are
// excluded.
func gatewayCallSitesIn(t *testing.T, filename string, src []byte) []gatewayCallSite {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	var sites []gatewayCallSite
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Recv != nil {
			continue
		}
		body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		if !gatewayDispatch.MatchString(body) {
			continue
		}
		sites = append(sites, gatewayCallSite{
			file: filename, fn: fn.Name.Name,
			begins:   strings.Contains(body, "beginLLMCall("),
			finishes: strings.Count(body, "finishLLMCall("),
		})
	}
	return sites
}

// checkGatewayCallSite reports every way one dispatching function fails §4.6.
func checkGatewayCallSite(s gatewayCallSite) []string {
	var problems []string
	if !s.begins {
		problems = append(problems, "dispatches without beginLLMCall — §4.6(1) requires an execution_id BEFORE dispatch, so an unrecorded call cannot be reconciled later")
	}
	if s.finishes == 0 {
		problems = append(problems, "dispatches without finishLLMCall — §4.6(2,3) require the usage record and its cost")
	} else if s.finishes < 2 {
		// Recording only the happy path is the exact failure mode §4.6 exists
		// for: the pod died on the expensive run. A gateway function has at
		// least one error branch and one success branch, so one call site
		// means one of them is silent.
		problems = append(problems, "calls finishLLMCall once — a gateway call that only records when it SUCCEEDS loses exactly the calls that cost money and returned nothing")
	}
	return problems
}

// The two live tests above can only afford to exercise the entry points they
// pay for. This one covers the REST of §4.6's word "every": it reads
// api/utils and insists that every place a request leaves the process for the
// gateway sits inside a function that mints an execution id BEFORE dispatch
// and reconciles the row afterwards, on both branches.
//
// It is a source guard rather than a runtime one because the failure it
// catches is a NEW gateway endpoint — a moderation call, a vision call, an
// image call — added by someone who copied the HTTP plumbing and not the
// bookkeeping. That call would never appear in a cost report, and nothing
// else in this suite would notice.
func TestEveryGatewayCallSiteRecordsItsCost(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "api", "utils")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	var sites []gatewayCallSite
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		sites = append(sites, gatewayCallSitesIn(t, name, src)...)
	}

	for _, s := range sites {
		for _, p := range checkGatewayCallSite(s) {
			t.Errorf("api/utils/%s: %s %s", s.file, s.fn, p)
		}
	}
	if len(sites) == 0 {
		t.Fatal("no gateway dispatch site found in api/utils — this guard is not looking where the calls are")
	}
	t.Logf("%d gateway entry point(s), every one recorded before dispatch and reconciled on both branches", len(sites))
}

// A guard that cannot fail is decoration. This is the unrecorded gateway call
// somebody adds next quarter, and the guard must reject it.
func TestGatewayCostGuardActuallyCatches(t *testing.T) {
	const unrecorded = `package utils

func Moderate(ctx context.Context, text string) (bool, error) {
	g := gatewayHandle()
	raw, err := g.post(ctx, "/moderations", map[string]any{"input": text})
	if err != nil {
		return false, err
	}
	return len(raw) > 0, nil
}
`
	sites := gatewayCallSitesIn(t, "moderation.go", []byte(unrecorded))
	if len(sites) != 1 {
		t.Fatalf("the guard found %d dispatch sites in an obvious one — it is not looking at function bodies", len(sites))
	}
	if problems := checkGatewayCallSite(sites[0]); len(problems) < 2 {
		t.Fatalf("an entirely unrecorded gateway call raised %d problem(s), want both the missing begin and the missing finish: %v",
			len(problems), problems)
	}

	// Half-done bookkeeping is the more likely mistake: the success path
	// records, the error path returns early and the money vanishes.
	const successOnly = `package utils

func Vision(ctx context.Context, model string, ref CallRef) error {
	executionID, callID := beginLLMCall(model, "vision", ref)
	g := gatewayHandle()
	raw, err := g.post(ctx, "/chat/completions", nil)
	if err != nil {
		return err
	}
	finishLLMCall(callID, "", "succeeded", usageFrom(nil), model, "vision")
	_ = raw
	return nil
}
`
	sites = gatewayCallSitesIn(t, "vision.go", []byte(successOnly))
	if len(sites) != 1 {
		t.Fatalf("found %d dispatch sites, want 1", len(sites))
	}
	problems := checkGatewayCallSite(sites[0])
	if len(problems) != 1 {
		t.Fatalf("a call that records only on success raised %d problem(s), want exactly 1: %v", len(problems), problems)
	}

	// And it must NOT fire on a correctly-written one, or it gets deleted.
	const correct = `package utils

func Embed(ctx context.Context, model string, ref CallRef) error {
	executionID, callID := beginLLMCall(model, "embed", ref)
	g := gatewayHandle()
	raw, err := g.post(ctx, "/embeddings", nil)
	if err != nil {
		finishLLMCall(callID, "", "failed", llmUsage{}, model, "embed")
		return err
	}
	finishLLMCall(callID, "", "succeeded", usageFrom(nil), model, "embed")
	_ = raw
	return nil
}
`
	sites = gatewayCallSitesIn(t, "embeddings.go", []byte(correct))
	if len(sites) != 1 {
		t.Fatalf("found %d dispatch sites, want 1", len(sites))
	}
	if problems := checkGatewayCallSite(sites[0]); len(problems) != 0 {
		t.Fatalf("the guard FALSE-POSITIVES on a correctly recorded gateway call: %v", problems)
	}

	// A function that touches no gateway is not a call site at all.
	const unrelated = `package utils

func DossierKey(tenantID, dossierID, name string) string {
	return BlobPrefix() + tenantID + "/" + dossierID + "/" + name
}
`
	if sites := gatewayCallSitesIn(t, "blob.go", []byte(unrelated)); len(sites) != 0 {
		t.Fatalf("the guard treated %d non-gateway function(s) as dispatch sites", len(sites))
	}
}

// A SUCCESSFUL call must reconcile its llm_calls row with known usage and the
// provider-reported cost.
func TestCostAccountingSuccessfulCallRecordsUsage(t *testing.T) {
	testsupport.Boot(t)
	if !u.LLMConfigured() {
		t.Skip("OPENROUTER_API_KEY not set — the live cost-path test needs the real gateway")
	}
	firm := testsupport.NewFirm(t, "costok")
	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	vectors, err := u.Embed(u.WithCallRef(ctx, u.CallRef{TenantID: firm.ID}),
		cfg.EmbeddingModel, []string{"Le licenciement doit reposer sur une cause réelle et sérieuse."})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != cfg.EmbeddingDims {
		t.Fatalf("embedding shape = %d vectors of %d dims, want 1 x %d",
			len(vectors), len(vectors[0]), cfg.EmbeddingDims)
	}

	var call types.LLMCall
	if err := u.DB.Where("tenant_id = ? AND purpose = ?", firm.ID, types.LLMPurposeEmbed).
		Order("started_at DESC").Take(&call).Error; err != nil {
		t.Fatalf("no llm_calls row for the embedding call: %v", err)
	}
	if call.ExecutionID == "" {
		t.Fatal("llm_calls row has no execution_id")
	}
	if call.UsageStatus != types.UsageStatusKnown {
		t.Fatalf("usage_status = %q for a call that returned usage, want known", call.UsageStatus)
	}
	t.Logf("successful call: execution_id=%s usage_status=%s tokens=%d/%d cost=%d nano-USD",
		call.ExecutionID, call.UsageStatus, call.PromptTokens, call.CompletionTokens, call.CostNanoUSD)
}

// Nothing anywhere may invent usage. This sweeps the WHOLE table rather than
// one row, because "usage_status=estimate" is the kind of shortcut that gets
// added later, under deadline, by someone who wants a nicer cost dashboard.
func TestCostAccountingNeverEstimatesUsage(t *testing.T) {
	testsupport.Boot(t)

	if n := testsupport.Count(t, "llm_calls", "usage_status = ?", types.UsageStatusEstimate); n != 0 {
		t.Fatalf("%d llm_calls rows carry usage_status=estimate — §4.6 forbids it: a character-count estimate is not auditable billing", n)
	}
	if n := testsupport.Count(t, "llm_calls",
		"usage_status = ? AND (prompt_tokens <> 0 OR completion_tokens <> 0)",
		types.UsageStatusUnknown); n != 0 {
		t.Fatalf("%d llm_calls rows claim unknown usage but carry token counts — those numbers came from somewhere", n)
	}
	if n := testsupport.Count(t, "llm_calls",
		"usage_status NOT IN (?, ?)", types.UsageStatusKnown, types.UsageStatusUnknown); n != 0 {
		t.Fatalf("%d llm_calls rows carry an unknown usage_status value", n)
	}

	// A cost is the provider's report of a KNOWN usage; a call whose usage is
	// not known carrying one would be a bill for a number nobody measured.
	if n := testsupport.Count(t, "llm_calls",
		"usage_status <> ? AND cost_nano_usd <> 0", types.UsageStatusKnown); n != 0 {
		t.Fatalf("%d llm_calls rows carry a cost for usage that is not known — that is a bill for a number nobody measured", n)
	}
	t.Log("no estimated usage anywhere; no call bills an unmeasured usage")
}

// ---------------------------------------------------------------------------
// 4. IDEMPOTENT PUBLISH (§4.3)
// ---------------------------------------------------------------------------

// "Exports write to a temp key, validate, then publish an immutable artifact
// row" (§4.3), and S3 keys "derive from tenant + dossier + artifact kind +
// content hash or step id". Put together, those two sentences promise that a
// killed-and-replayed export converges: ONE artifact row, ONE object.
//
// Replaying an export is not hypothetical. Execution is at-least-once: an
// attempt that published its bytes and its rows and then lost its lease before
// finishSuccess committed WILL be handed to another worker (see
// TestAtLeastOnceNotExactlyOnce, which proves exactly that sequence). The
// runner's conditional publish discards the stranded attempt's OUTPUT — it
// cannot un-write the artifact row that attempt already committed.
//
// This test calls BuildExport twice with the same run id, which is precisely
// what that replay does.
func TestIdempotentPublishExportReplay(t *testing.T) {
	testsupport.Boot(t)
	if _, err := services.PandocVersion(context.Background()); err != nil {
		t.Skipf("pandoc not available: %v", err)
	}
	blob := testsupport.NewMemBlob(t)
	firm := testsupport.NewFirm(t, "idempub")
	repo := firm.Repo()

	d := types.Dossier{ID: uuid.NewString(), Title: "Licenciement — replay", State: types.DossierStateActive, CreatedBy: firm.UserID}
	if err := repo.Create(&d); err != nil {
		t.Fatalf("seeding the dossier: %v", err)
	}
	assetID := uuid.NewString()
	key := u.DossierKey(firm.ID, d.ID, assetID+"-contrat.pdf")
	if err := u.Blob.Put(context.Background(), key, strings.NewReader("%PDF-1.7 original bytes"), 23, "application/pdf"); err != nil {
		t.Fatalf("staging the original: %v", err)
	}
	if err := repo.Create(&types.Asset{
		ID: assetID, S3Key: key, Kind: types.AssetKindOriginal, Mime: "application/pdf",
		Size: 23, SHA256: strings.Repeat("c", 64), CreatedBy: firm.UserID,
	}); err != nil {
		t.Fatalf("seeding the asset: %v", err)
	}
	if err := repo.Create(&types.Piece{
		ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: assetID,
		OriginalName: "contrat.pdf", Ordinal: 1, PieceKind: "contrat",
		Title: "Contrat de travail", ClassificationState: types.PieceStateApproved,
	}); err != nil {
		t.Fatalf("seeding the piece: %v", err)
	}

	runID := uuid.NewString()
	first, err := services.BuildExport(context.Background(), repo, d, services.ExportOptions{RunID: runID})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	objectsAfterFirst := len(blob.Keys(""))

	// The replay. Same dossier, same run, same inputs — the attempt that took
	// over from a worker that vanished after publishing.
	second, err := services.BuildExport(context.Background(), repo, d, services.ExportOptions{RunID: runID})
	if err != nil {
		t.Fatalf("replayed export: %v", err)
	}

	if second.ExportSHA256 != first.ExportSHA256 {
		t.Fatalf("the replay produced different bytes (%s vs %s) — an export must be reproducible",
			first.ExportSHA256[:12], second.ExportSHA256[:12])
	}
	if got := testsupport.Count(t, "artifacts", "tenant_id = ? AND run_id = ? AND kind = ?",
		firm.ID, runID, types.ArtifactKindExportZip); got != 1 {
		t.Fatalf("export_zip artifact rows after a replay = %d, want exactly 1", got)
	}
	if got := testsupport.Count(t, "artifacts", "tenant_id = ? AND run_id = ? AND kind = ?",
		firm.ID, runID, types.ArtifactKindBordereauDocx); got != 1 {
		t.Fatalf("bordereau artifact rows after a replay = %d, want exactly 1", got)
	}
	if got := len(blob.Keys("")); got != objectsAfterFirst {
		t.Fatalf("the replay left %d blob objects, want %d — a replayed publish must overwrite, not accumulate",
			got, objectsAfterFirst)
	}
	if first.ArtifactID != second.ArtifactID {
		t.Fatalf("the replay published a different artifact id (%s vs %s)", first.ArtifactID, second.ArtifactID)
	}
}

// The half of idempotent publication that DOES hold today, kept as a live
// regression test: an export publishes its artifact row only AFTER the bytes
// are written and re-read from storage, so nothing listed by
// GET /runs/:id/artifacts can be undownloadable (§4.3, §10).
func TestPublishHappensAfterTheBytesExist(t *testing.T) {
	testsupport.Boot(t)
	if _, err := services.PandocVersion(context.Background()); err != nil {
		t.Skipf("pandoc not available: %v", err)
	}
	blob := testsupport.NewMemBlob(t)
	firm := testsupport.NewFirm(t, "publishorder")
	repo := firm.Repo()

	d := types.Dossier{ID: uuid.NewString(), Title: "Ordre de publication", State: types.DossierStateActive, CreatedBy: firm.UserID}
	if err := repo.Create(&d); err != nil {
		t.Fatalf("seeding the dossier: %v", err)
	}
	assetID := uuid.NewString()
	key := u.DossierKey(firm.ID, d.ID, assetID+"-piece.pdf")
	if err := u.Blob.Put(context.Background(), key, strings.NewReader("%PDF-1.7 bytes"), 14, "application/pdf"); err != nil {
		t.Fatalf("staging the original: %v", err)
	}
	if err := repo.Create(&types.Asset{
		ID: assetID, S3Key: key, Kind: types.AssetKindOriginal, Mime: "application/pdf",
		Size: 14, SHA256: strings.Repeat("d", 64), CreatedBy: firm.UserID,
	}); err != nil {
		t.Fatalf("seeding the asset: %v", err)
	}
	if err := repo.Create(&types.Piece{
		ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: assetID,
		OriginalName: "piece.pdf", Ordinal: 1, Title: "Pièce n°1",
		ClassificationState: types.PieceStateApproved,
	}); err != nil {
		t.Fatalf("seeding the piece: %v", err)
	}

	runID := uuid.NewString()
	res, err := services.BuildExport(context.Background(), repo, d, services.ExportOptions{RunID: runID})
	if err != nil {
		t.Fatalf("BuildExport: %v", err)
	}

	// Every published artifact row must resolve to an asset whose object is
	// really in storage. This is the property that makes "an artifact row
	// exists" a safe thing for the UI to offer a download button for.
	var arts []types.Artifact
	if err := repo.Artifacts().Where("run_id = ?", runID).Find(&arts).Error; err != nil {
		t.Fatalf("listing artifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("published artifacts = %d, want 2 (the zip and the bordereau)", len(arts))
	}
	for _, a := range arts {
		var asset types.Asset
		if err := repo.Assets().Where("id = ?", a.AssetID).Take(&asset).Error; err != nil {
			t.Fatalf("artifact %s (%s) points at a missing asset row: %v", a.ID, a.Kind, err)
		}
		rc, err := u.Blob.Get(context.Background(), asset.S3Key)
		if err != nil {
			t.Fatalf("artifact %s (%s) is listed but its object is not in storage: %v", a.ID, a.Kind, err)
		}
		rc.Close()
		if !strings.HasPrefix(asset.S3Key, u.BlobPrefix()) {
			t.Fatalf("artifact %s was published outside the app prefix: %s", a.ID, asset.S3Key)
		}
		if !strings.Contains(asset.S3Key, firm.ID) {
			t.Fatalf("artifact %s key does not carry the tenant: %s", a.ID, asset.S3Key)
		}
	}

	// And the temp key the two-phase publish wrote to must be gone: a
	// half-published export must leave nothing visible.
	for _, k := range blob.Keys("") {
		if strings.Contains(k, "tmp-export-") {
			t.Fatalf("the temporary export object %s was left behind", k)
		}
	}

	// An export nobody approved is stamped as a draft (§13).
	if res.Approved {
		t.Fatal("an export was published as approved without an approval")
	}
	if res.DraftNotice != services.DraftNotice {
		t.Fatalf("draft notice = %q, want %q", res.DraftNotice, services.DraftNotice)
	}
	t.Logf("published %d artifacts, all downloadable, no temp object left, notice=%q", len(arts), res.DraftNotice)
}
