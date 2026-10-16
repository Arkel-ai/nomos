// Package runner is the workflow runner: the claimer, the differentiated
// worker pools, the reaper, and the registry the code-owned workflow
// definitions plug into.
//
// It is a narrow Postgres queue and it must not be described internally as a
// workflow engine (docs/PLAN.md §4.7). River would be the right library if the
// "AutoMigrate only" rule is ever relaxed; go-workflows is a far larger
// commitment; Asynq and Machinery need Redis, which is out of bounds. So:
// goroutines inside the API process, and Postgres as the queue.
//
// # The three invariants everything else depends on
//
//  1. Claiming is ONE SHORT TRANSACTION that commits before any external call
//     — UPDATE … WHERE state='queued' AND available_at <= now() … FOR UPDATE
//     SKIP LOCKED, inserting the step_attempts and run_events rows in the same
//     transaction. Never hold a row lock or a transaction across OCR, S3,
//     pandoc or an LLM call (§4.2). See claim.go.
//  2. Leases are the only real recovery mechanism, because the pod can die
//     mid-mission and neither the 10 s graceful shutdown nor Kubernetes' 30 s
//     termination window survives node loss (§2.1). ~90 s lease, 20 s
//     heartbeat, and the reaper requeues only after expiry with a conditional
//     update on the OLD lease owner so a slow-but-alive worker is never
//     double-claimed. See reaper.go.
//  3. Execution is AT-LEAST-ONCE, NEVER EXACTLY-ONCE (§2.3).
//
// # Why at-least-once, and what that costs
//
// Exactly-once would require the external side effect (an OpenRouter
// completion, an S3 PUT, a pandoc render) and the Postgres row that records it
// to commit atomically. They are different systems with no shared transaction,
// so there is always a window: the provider accepted the call, the pod died
// before the llm_calls row committed, and the reaper hands the step to another
// worker. That retry is a double-spend and a possibly-different answer. It is
// a NAMED, ACCEPTED failure mode, not a bug to be fixed later.
//
// What makes it survivable is that every step carries a stable
// idempotency_key while every attempt carries a distinct execution_id:
//
//   - the idempotency_key is what an internal side effect converges on — an S3
//     key derived from tenant+dossier+kind+hash, a unique constraint, an
//     upsert — so a replayed attempt OVERWRITES rather than duplicates;
//   - the execution_id is what an external call is tagged with, so a
//     double-spend is at least attributable and reconcilable afterwards;
//   - publishing a step's output is a CONDITIONAL transition
//     (WHERE state='running' AND lease_owner=<me>), so a worker that lost its
//     lease mid-flight silently discards its result instead of clobbering the
//     attempt that took over.
//
// Steps must therefore be small, idempotent and leased. A step that cannot be
// replayed safely is a step that does not belong in this runner.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var log = u.NewLogger("runner")

// Tunables. Package vars rather than constants so the tests can compress the
// clock; production overrides come from config (pool sizes) and the registry
// (per-kind lease and max attempts).
var (
	// DefaultLease is the lease a claimed step holds before the reaper may
	// take it back. Configurable per step kind via RegisterOptions.Lease.
	DefaultLease = 90 * time.Second
	// HeartbeatInterval must stay comfortably under DefaultLease. If a
	// heartbeat write fails we cancel the step's context so the external
	// call is aborted BEFORE the lease can be reclaimed elsewhere.
	HeartbeatInterval = 20 * time.Second
	// PollInterval is how long a pool waits before looking for work again
	// when the last claim came back empty. Enqueue nudges the pools awake,
	// so this is a safety net, not the latency budget.
	PollInterval = 2 * time.Second
	// ReaperInterval is how often expired leases are swept.
	ReaperInterval = 15 * time.Second
	// ClaimBatch bounds one claim transaction. Bounded on purpose: the
	// transaction must be short.
	ClaimBatch = 10
	// BaseBackoff / MaxBackoff shape the capped exponential retry delay.
	BaseBackoff = 3 * time.Second
	MaxBackoff  = 5 * time.Minute
	// DefaultMaxAttempts is used when a step row does not set its own.
	DefaultMaxAttempts = 3
)

// workerID identifies this process in run_steps.lease_owner and
// step_attempts.worker_id. Host + pid is not unique across restarts of the
// same pod name, so a random suffix is appended: reclaiming a lease must never
// match a dead predecessor's owner string.
var workerID = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8])
}()

// WorkerID returns this process's lease owner string.
func WorkerID() string { return workerID }

// StepFunc is the unit of work behind one step kind. It receives a context
// that is cancelled when the lease is lost, when cancellation is requested, or
// when the process shuts down — every external call MUST honour it.
//
// The returned output is stored on the step row as jsonb. Returning
// ErrWaitingInput parks the step (see questions.go); returning a *StepError
// with Retryable=false fails it immediately without burning the remaining
// attempts.
type StepFunc func(ctx context.Context, sc *StepCtx) (output any, err error)

// StepError is a step failure with an operator-facing code. Codes are short
// snake_case tokens; they land in run_steps.error_code and in the
// step.failed event, so they must never carry a prompt, document text or a
// presigned URL (§7.4).
type StepError struct {
	Code      string
	Message   string
	Retryable bool
	Err       error
}

func (e *StepError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *StepError) Unwrap() error { return e.Err }

// Retry returns a retryable failure: a timeout, a 429, a transient 5xx.
func Retry(code, message string) error {
	return &StepError{Code: code, Message: message, Retryable: true}
}

// Permanent returns a failure that must NOT be retried: schema-invalid input,
// a policy refusal, context-too-large, a deterministic parse failure. Retrying
// those without changing the input only burns money (§4.3).
func Permanent(code, message string) error {
	return &StepError{Code: code, Message: message, Retryable: false}
}

// classify maps an arbitrary step error onto (code, summary, retryable).
func classify(err error) (code, summary string, retryable bool) {
	var se *StepError
	if errors.As(err, &se) {
		msg := se.Message
		if msg == "" {
			msg = se.Code
		}
		return se.Code, msg, se.Retryable
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "aborted", "step context cancelled", true
	}
	return "step_error", err.Error(), true
}

// StepCtx is what a StepFunc is handed: the run, the step row, the attempt,
// the tenant and dossier scope, and the three things a long step needs —
// Heartbeat, Emit and Cancelled.
type StepCtx struct {
	Run     types.Run
	Step    types.RunStep
	Attempt types.StepAttempt

	// TenantID and DossierID are lifted out of the run for convenience;
	// tenant_id must be in every private query, every S3 key and every
	// Casbin domain (§9.1(12)).
	TenantID  string
	DossierID string

	// ExecutionID is this attempt's id. It is minted BEFORE dispatch and is
	// what an llm_calls row is keyed on (§4.6).
	ExecutionID string
	WorkerID    string

	db        *gorm.DB
	cancel    context.CancelCauseFunc
	cancelled atomic.Bool
	// parked carries the questions an ask_user step wrote, so the runner can
	// park the step in the same transition that closes the attempt.
	parked bool
}

// DB is the handle a step should use for its own writes. It is the plain
// package handle: a step must not hold a transaction across an external call.
func (sc *StepCtx) DB() *gorm.DB { return sc.db }

// Input decodes the step's jsonb input into dest. A step with no input is not
// an error; dest is simply left untouched.
func (sc *StepCtx) Input(dest any) error {
	if len(sc.Step.Input) == 0 {
		return nil
	}
	if err := json.Unmarshal([]byte(sc.Step.Input), dest); err != nil {
		return Permanent("invalid_step_input", "step input does not match the expected shape")
	}
	return nil
}

// Cancelled reports whether cancellation has been observed for this run. It
// reads a flag refreshed on every heartbeat plus the claim-time check, so it
// is cheap enough to call inside a loop. The durable truth is
// runs.cancel_requested_at — this is only the cached view of it (§4.7 → cancel.go).
func (sc *StepCtx) Cancelled() bool { return sc.cancelled.Load() }

// Heartbeat extends the lease and re-reads the cancellation flag, both against
// the POSTGRES clock. It returns an error when the lease is no longer ours —
// the caller (and the runner's own heartbeat goroutine) must then abandon the
// work, because another worker is entitled to claim it.
func (sc *StepCtx) Heartbeat() error {
	lease := leaseFor(sc.Step.Kind)
	res := sc.db.Exec(`
		UPDATE run_steps
		   SET heartbeat_at      = now(),
		       lease_expires_at  = now() + make_interval(secs => ?::double precision),
		       updated_at        = EXTRACT(EPOCH FROM now())::bigint
		 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?`,
		lease.Seconds(), sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("lease lost for step %s", sc.Step.ID)
	}
	if cancelled, err := cancelRequested(sc.db, sc.Run.ID); err == nil && cancelled {
		sc.cancelled.Store(true)
	}
	return nil
}

// Emit appends one progress event. run_events is the user-visible log and the
// cursor the thread replays from — never security evidence, and never document
// content (§4.5).
func (sc *StepCtx) Emit(eventType string, payload any) error {
	stepID := sc.Step.ID
	return emitEvent(sc.db, &sc.Run, &stepID, eventType, payload)
}

// Start launches the runner goroutines and returns immediately. They run until
// ctx is cancelled; main.go cancels it BEFORE srv.Shutdown so the runner stops
// claiming new steps while in-flight requests drain.
//
// Pools are differentiated, not one shared pool (§4.2): ocr and render are
// CPU-bound and run one at a time per pod, llm steps are slow but idle, db
// steps are cheap. A single pool would let OCR starve request handling and let
// cheap transitions queue behind three-minute calls.
func Start(ctx context.Context, cfg config.Config) {
	if u.DB == nil {
		log.Info("Postgres not configured — workflow runner not started")
		return
	}

	log.Infof("Workflow runner starting (worker=%s pools: ocr=%d render=%d llm=%d db=%d)",
		workerID, cfg.StepConcurrencyOCR, cfg.StepConcurrencyRender,
		cfg.StepConcurrencyLLM, cfg.StepConcurrencyDB)

	for _, p := range poolsFrom(cfg) {
		go p.run(ctx, u.DB)
	}
	go reaperLoop(ctx, u.DB)

	go func() {
		<-ctx.Done()
		log.Info("Workflow runner stopped")
	}()
}

// execute runs one already-claimed step: heartbeat goroutine, the StepFunc,
// then the conditional publish. The claim transaction has already committed —
// nothing here holds a row lock or a transaction across the call.
func execute(ctx context.Context, db *gorm.DB, sc *StepCtx) {
	fn, ok := lookup(sc.Step.Kind)
	if !ok {
		log.Errorf("Step kind %q has no registered function (step=%s run=%s)", sc.Step.Kind, sc.Step.ID, sc.Run.ID)
		finishFailure(db, sc, "unknown_step_kind", "no handler registered for this step kind", false)
		return
	}

	stepCtx, cancel := context.WithCancelCause(ctx)
	sc.cancel = cancel
	defer cancel(nil)

	// Heartbeat every HeartbeatInterval. A failed heartbeat means the lease
	// is gone (or Postgres is): cancel the step's context so the external
	// call is aborted BEFORE the lease can be reclaimed elsewhere (§4.2).
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stepCtx.Done():
				return
			case <-t.C:
				if err := sc.Heartbeat(); err != nil {
					log.Warnf("Heartbeat failed (step=%s run=%s): %v — aborting the step", sc.Step.ID, sc.Run.ID, err)
					cancel(err)
					return
				}
				if sc.Cancelled() {
					log.Infof("Cancellation observed at heartbeat (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
					cancel(context.Canceled)
					return
				}
			}
		}
	}()

	output, err := fn(stepCtx, sc)
	cancel(nil)
	<-hbDone

	switch {
	case errors.Is(err, ErrWaitingInput):
		park(db, sc)
	case err != nil:
		code, summary, retryable := classify(err)
		log.Warnf("Step failed (run=%s step=%s kind=%s attempt=%d code=%s retryable=%t)",
			sc.Run.ID, sc.Step.ID, sc.Step.Kind, sc.Step.AttemptCount, code, retryable)
		finishFailure(db, sc, code, summary, retryable)
	default:
		finishSuccess(db, sc, output)
	}
}

// Expansion is what a step returns when its success GROWS THE PLAN: the
// output it publishes, and the steps that join the run in the SAME
// transaction as that success. Both commit or neither does — a crash or a lost
// lease never leaves children claimable without the output that justified
// them, nor an accepted output without its children, nor a run that looks
// finished in between. EnqueueSteps followed by a successful return cannot
// promise that: the two writes commit apart.
//
// Children may depend on the expanding step's own key, on any step already in
// the run, or on a child listed EARLIER in Steps — never on a later one, which
// is what makes a cycle impossible. Their keys must be new to the run; a
// collision is an integrity failure, never a merge.
type Expansion struct {
	Output any
	Steps  []StepSpec
	// Commit, when set, runs inside the same lease-fenced transaction that
	// publishes Output and inserts Steps — after the conditional transition,
	// before the children. It is how a step's private rows (an agent's
	// revisions, evidence, activity events) become durable together with
	// the success that accepts them. It must do no HTTP, S3, pandoc or model
	// work. An error rolls everything back and fails the attempt
	// (retryable). A cancelled or already-closed run skips it entirely.
	Commit func(tx *gorm.DB) error
	// Quiet skips the run.plan_ready event. A run whose progress the thread
	// shows some other way would otherwise re-send its whole, growing plan
	// on every step.
	Quiet bool
}

// Terminal is what a step returns when its success ENDS THE RUN with a stated
// outcome — a turn limit, a deadline, a failure the step has already
// explained. Output and Commit behave as in Expansion; then, in the same
// transaction, every step still queued or parked is cancelled and the run is
// closed in RunState with FailureCode, followed by its terminal hook. A step
// that ends a run this way can never be mistaken for a success because the
// last remaining step happened to return nil.
type Terminal struct {
	Output any
	// RunState is types.RunStateFailed (the default when empty) or
	// types.RunStateSucceeded.
	RunState    string
	FailureCode string
	Commit      func(tx *gorm.DB) error
}

// errExpansionConflict marks an expansion whose step keys collide with the
// run's. Replays cannot reach it — the conditional transition misses first —
// so it is a defect in the step, and it fails the step permanently.
var errExpansionConflict = errors.New("runner: expansion step key already in the run")

// errExpansionInvalid marks an expansion whose dependencies name a step that
// does not exist yet (or a later child, which could close a cycle).
var errExpansionInvalid = errors.New("runner: expansion depends on an unknown or later step")

// errStepLimit marks an expansion that would grow the run past MaxRunSteps.
var errStepLimit = errors.New("runner: run step limit reached")

// commitError wraps a failure of a step's own Commit hook, so it is told apart
// from a lost lease or a broken publication.
type commitError struct{ err error }

func (e *commitError) Error() string { return "runner: step commit failed: " + e.err.Error() }
func (e *commitError) Unwrap() error { return e.err }

// MaxRunSteps bounds how far expansions may grow one run. A run is a few
// dozen steps; a thousand is a loop that forgot to stop.
var MaxRunSteps = 1000

// finishSuccess publishes the step's output as a CONDITIONAL transition:
//
//	WHERE state='running' AND lease_owner=<me> AND attempt_count=<my attempt>
//
// attempt_count is part of the predicate, not just lease_owner, because two
// attempts of the same step can be running in the SAME process (the reaper
// reclaimed a lease this pod still holds) and would otherwise share a
// lease_owner string. If the predicate misses, a replayed attempt has taken
// over and this result is discarded rather than duplicated.
//
// Cancellation is checked one last time here — before publishing — so a run
// the lawyer stopped does not silently gain a new output. It is read UNDER THE
// RUN ROW LOCK, taken first, which is the lock RequestCancel holds while it
// stamps the flag: a cancellation is either committed before this publication
// (and seen) or waits for it. The attempt row and any llm_calls rows are left
// exactly as they are: cancelling never erases completed evidence or cost
// records.
func finishSuccess(db *gorm.DB, sc *StepCtx, output any) {
	attemptState := types.AttemptStateSucceeded

	var (
		expansion []StepSpec
		commit    func(tx *gorm.DB) error
		quiet     bool
		terminal  *Terminal
	)
	switch o := output.(type) {
	case Expansion:
		output, expansion, commit, quiet = o.Output, o.Steps, o.Commit, o.Quiet
	case Terminal:
		t := o
		output, commit, terminal = o.Output, o.Commit, &t
	}

	payload, err := toJSONB(output)
	if err != nil {
		log.Errorf("Step output is not JSON-encodable (step=%s): %v", sc.Step.ID, err)
		finishFailure(db, sc, "unencodable_output", "step output could not be encoded", false)
		return
	}

	state := types.StepStateSucceeded
	cancelled := false
	err = db.Transaction(func(tx *gorm.DB) error {
		run, err := lockRun(tx, sc.Run.ID)
		if err != nil {
			return err
		}
		cancelled = run.CancelRequestedAt != nil
		if cancelled {
			state = types.StepStateCancelled
		}
		res := tx.Exec(`
			UPDATE run_steps
			   SET state = ?, output = ?, error_code = '', error_summary = '',
			       lease_owner = '', lease_expires_at = NULL,
			       finished_at = EXTRACT(EPOCH FROM now())::bigint,
			       updated_at  = EXTRACT(EPOCH FROM now())::bigint
			 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?`,
			state, payload, sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Lease lost. Another attempt owns the step; discard.
			return errLeaseLost
		}
		if err := closeAttempt(tx, sc, attemptState, "", true); err != nil {
			return err
		}
		evType := types.EventStepSucceeded
		if cancelled {
			evType = types.EventStepFailed
		}
		stepID := sc.Step.ID
		if err := emitEvent(tx, &sc.Run, &stepID, evType, map[string]any{
			"step_key": sc.Step.StepKey,
			"kind":     sc.Step.Kind,
			"label":    sc.Step.Label,
			"attempt":  sc.Step.AttemptCount,
			"state":    state,
		}); err != nil {
			return err
		}
		// A cancelled or already-closed run accepts nothing beyond the
		// step's own transition: no private rows, no children, no outcome.
		if cancelled || types.TerminalRunState(run.State) {
			return refreshRunState(tx, sc.Run.ID)
		}
		if commit != nil {
			if err := commit(tx); err != nil {
				return &commitError{err: err}
			}
		}
		if terminal != nil {
			runState := terminal.RunState
			if runState == "" {
				runState = types.RunStateFailed
			}
			return closeRun(tx, run, runState, terminal.FailureCode)
		}
		if len(expansion) > 0 {
			if err := expand(tx, sc, expansion, quiet); err != nil {
				return err
			}
		}
		return refreshRunState(tx, sc.Run.ID)
	})
	var ce *commitError
	switch {
	case errors.As(err, &ce):
		log.Errorf("Step commit failed, nothing accepted (run=%s step=%s kind=%s): %v",
			sc.Run.ID, sc.Step.ID, sc.Step.Kind, ce.err)
		finishFailure(db, sc, "commit_failed", "the step's accepted state could not be committed", true)
	case errors.Is(err, errExpansionConflict):
		log.Errorf("Expansion rejected: step key already in the run (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
		finishFailure(db, sc, "expansion_conflict", "the step tried to add a step key the run already has", false)
	case errors.Is(err, errExpansionInvalid):
		log.Errorf("Expansion rejected: dependency on an unknown or later step (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
		finishFailure(db, sc, "expansion_invalid", "the step tried to add a step depending on an unknown step", false)
	case errors.Is(err, errStepLimit):
		log.Errorf("Expansion rejected: run step limit reached (run=%s step=%s limit=%d)", sc.Run.ID, sc.Step.ID, MaxRunSteps)
		finishFailure(db, sc, "step_limit", "the run reached its step limit", false)
	case errors.Is(err, errLeaseLost):
		log.Warnf("Discarded output: lease lost before publish (run=%s step=%s attempt=%d)",
			sc.Run.ID, sc.Step.ID, sc.Step.AttemptCount)
	case err != nil:
		log.Errorf("Publishing step output failed (run=%s step=%s): %v", sc.Run.ID, sc.Step.ID, err)
	default:
		log.Infof("Step done (run=%s step=%s kind=%s state=%s attempt=%d added=%d terminal=%t)",
			sc.Run.ID, sc.Step.ID, sc.Step.Kind, state, sc.Step.AttemptCount, len(expansion), terminal != nil)
		if len(expansion) > 0 {
			notifyPools()
		}
	}
}

// expand inserts an Expansion's steps inside finishSuccess's transaction and
// publishes the whole plan. A run that is already terminal gains nothing.
//
// Every dependency must name a step already in the run or a child listed
// before it: with that ordering no cycle can form, and a dependency on a key
// that does not exist is refused instead of silently never resolving.
func expand(tx *gorm.DB, sc *StepCtx, specs []StepSpec, quiet bool) error {
	var run types.Run
	if err := tx.Where("id = ?", sc.Run.ID).Take(&run).Error; err != nil {
		return err
	}
	if types.TerminalRunState(run.State) {
		log.Warnf("Expansion dropped: run is %s (run=%s step=%s)", run.State, run.ID, sc.Step.ID)
		return nil
	}
	var existing []string
	if err := tx.Model(&types.RunStep{}).Where("run_id = ?", run.ID).
		Pluck("step_key", &existing).Error; err != nil {
		return err
	}
	if len(existing)+len(specs) > MaxRunSteps {
		return errStepLimit
	}
	known := make(map[string]bool, len(existing)+len(specs))
	for _, key := range existing {
		known[key] = true
	}
	for _, spec := range specs {
		if known[spec.Key] {
			return errExpansionConflict
		}
		for _, dep := range spec.DependsOn {
			if !known[dep] {
				return errExpansionInvalid
			}
		}
		known[spec.Key] = true
	}
	created, err := insertSteps(tx, run, specs)
	if err != nil {
		return err
	}
	if quiet {
		return nil
	}
	plan, err := planPayload(tx, run.ID)
	if err != nil {
		return err
	}
	return emitEvent(tx, &run, nil, types.EventPlanReady, map[string]any{
		"steps":       plan,
		"steps_added": len(created),
	})
}

// finishFailure either requeues the step with capped exponential backoff or
// exhausts it into state=failed with an error_code. Never an infinite loop
// (§4.3): max_attempts is a hard stop, and "retry from this step" is a new
// attempt with its own audit trail, not a silent reset.
func finishFailure(db *gorm.DB, sc *StepCtx, code, summary string, retryable bool) {
	max := sc.Step.MaxAttempts
	if max <= 0 {
		max = DefaultMaxAttempts
	}
	willRetry := retryable && sc.Step.AttemptCount < max
	backoff := backoffFor(sc.Step.AttemptCount)

	err := db.Transaction(func(tx *gorm.DB) error {
		var res *gorm.DB
		if willRetry {
			res = tx.Exec(`
				UPDATE run_steps
				   SET state = ?, error_code = ?, error_summary = ?,
				       lease_owner = '', lease_expires_at = NULL,
				       available_at = now() + make_interval(secs => ?::double precision),
				       updated_at = EXTRACT(EPOCH FROM now())::bigint
				 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?`,
				types.StepStateQueued, code, summary, backoff.Seconds(),
				sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount)
		} else {
			res = tx.Exec(`
				UPDATE run_steps
				   SET state = ?, error_code = ?, error_summary = ?,
				       lease_owner = '', lease_expires_at = NULL,
				       finished_at = EXTRACT(EPOCH FROM now())::bigint,
				       updated_at  = EXTRACT(EPOCH FROM now())::bigint
				 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?`,
				types.StepStateFailed, code, summary,
				sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount)
		}
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errLeaseLost
		}
		if err := closeAttempt(tx, sc, types.AttemptStateFailed, code, retryable); err != nil {
			return err
		}
		evType := types.EventStepFailed
		if willRetry {
			evType = types.EventStepRetrying
		}
		stepID := sc.Step.ID
		if err := emitEvent(tx, &sc.Run, &stepID, evType, map[string]any{
			"step_key":   sc.Step.StepKey,
			"kind":       sc.Step.Kind,
			"label":      sc.Step.Label,
			"attempt":    sc.Step.AttemptCount,
			"error_code": code,
			"retrying":   willRetry,
		}); err != nil {
			return err
		}
		return refreshRunState(tx, sc.Run.ID)
	})
	if errors.Is(err, errLeaseLost) {
		log.Warnf("Discarded failure: lease lost (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
		return
	}
	if err != nil {
		log.Errorf("Recording step failure failed (run=%s step=%s): %v", sc.Run.ID, sc.Step.ID, err)
	}
}

// closeAttempt finalises the step_attempts row for this execution.
func closeAttempt(tx *gorm.DB, sc *StepCtx, state, code string, retryable bool) error {
	return tx.Exec(`
		UPDATE step_attempts
		   SET state = ?, error_code = ?, retryable = ?,
		       finished_at = EXTRACT(EPOCH FROM now())::bigint
		 WHERE execution_id = ? AND state = ?`,
		state, code, retryable, sc.ExecutionID, types.AttemptStateRunning).Error
}

// backoffFor is capped exponential backoff with jitter derived from the
// execution id, so a fan-out of sibling steps does not retry in lockstep.
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := BaseBackoff << (attempt - 1)
	if d > MaxBackoff || d <= 0 {
		d = MaxBackoff
	}
	jitter := time.Duration(uuid.New().ID()%1000) * time.Millisecond
	return d + jitter
}

// ErrLeaseLost means this attempt no longer owns its step: another attempt
// took over, or the lease expired. Nothing it writes may be accepted.
var ErrLeaseLost = errors.New("runner: lease lost")

var errLeaseLost = ErrLeaseLost

// ErrRunCancelled means the lawyer asked to stop the run. A fenced write
// refuses it, so cancellation stops future work at the next commit.
var ErrRunCancelled = errors.New("runner: run cancellation requested")

// ErrRunClosed means the run already reached a terminal state.
var ErrRunClosed = errors.New("runner: run is closed")

// lockRun takes the run row lock. Every path that both reads the
// cancellation flag and changes a run's steps takes this lock FIRST — the
// publication of a step, a fenced write, RequestCancel, RecordAnswers — so
// they serialise in one order and a cancellation can never slip between a
// check and the write it guards.
func lockRun(tx *gorm.DB, runID string) (types.Run, error) {
	var run types.Run
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", runID).Take(&run).Error
	return run, err
}

// WithLeaseTransaction runs fn in a short transaction FENCED by this attempt's
// lease: it locks the run row and then the step row, and refuses
// (ErrRunCancelled, ErrRunClosed, ErrLeaseLost) unless the run is live, not
// cancelled, of this tenant, and the step is still running under this worker,
// this attempt and an unexpired lease — all against the Postgres clock.
//
// It is for the short writes a step must make durable BEFORE it finishes:
// reserving a model dispatch before the request leaves, recording the
// response before acting on it, appending a delivery checkpoint. fn must do
// no HTTP, S3, pandoc or model work; an error from fn or from the fence rolls
// everything back.
func (sc *StepCtx) WithLeaseTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return sc.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run, err := lockRun(tx, sc.Run.ID)
		if err != nil {
			return err
		}
		if run.TenantID != sc.TenantID {
			return ErrLeaseLost
		}
		if run.CancelRequestedAt != nil {
			return ErrRunCancelled
		}
		if types.TerminalRunState(run.State) {
			return ErrRunClosed
		}
		var held []string
		if err := tx.Raw(`
			SELECT id FROM run_steps
			 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?
			   AND lease_expires_at > now()
			   FOR UPDATE`,
			sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount).
			Scan(&held).Error; err != nil {
			return err
		}
		if len(held) == 0 {
			return ErrLeaseLost
		}
		return fn(tx)
	})
}

// Wake nudges the pools. Call it after committing steps through EnqueueTx.
func Wake() { notifyPools() }

// toJSONB marshals a step output for the jsonb column. A nil output stores
// SQL NULL rather than the string "null".
func toJSONB(v any) (types.JSONB, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return types.JSONB(b), nil
}
