package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// THE CLAIM.
//
// This is the single most important transaction in the repo. It is short, it
// touches only Postgres, and IT COMMITS BEFORE ANY EXTERNAL CALL. A 3-minute
// OCR / S3 / pandoc / LLM request behind an open transaction consumes a
// connection, retains row versions, blocks vacuum and fails badly during
// failover — so the claim hands the step to a worker only after COMMIT.
//
// What it does, atomically (§4.2):
//
//   - select a BOUNDED set of queued steps whose available_at has passed and
//     whose dependencies are all succeeded or skipped, FOR UPDATE SKIP LOCKED
//     so N replicas never fight over the same row;
//   - flip them to running, bump attempt_count, stamp lease_owner and
//     lease_expires_at = now() + <per-kind lease>;
//   - insert the step_attempts row (with the execution_id an LLM call will be
//     tagged with) and the run_events row IN THE SAME TRANSACTION.
//
// Every clock reference is now() — the POSTGRES clock. Pod clocks drift, and a
// lease compared against a drifting clock is not a lease.

// claimed is one step handed to a worker, with the run it belongs to.
type claimed struct {
	step    types.RunStep
	run     types.Run
	attempt types.StepAttempt
}

func claimSteps(ctx context.Context, db *gorm.DB, pool string, limit int) ([]claimed, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > ClaimBatch {
		limit = ClaimBatch
	}

	kindPred, kindArgs, ok := kindPredicate(pool)
	if !ok {
		return nil, nil
	}
	leaseExpr, leaseArgs := leaseExpression(pool)

	var out []claimed
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sql := `
		WITH candidate AS (
			SELECT s.id
			  FROM run_steps s
			  JOIN runs r ON r.id = s.run_id
			 WHERE s.state = ?
			   AND s.available_at <= now()
			   AND ` + kindPred + `
			   -- Cancellation is a DURABLE FLAG, checked before the claim.
			   -- A cancelled run stops being claimed on every replica at
			   -- once, with no in-memory state anywhere (§4.7 / cancel.go).
			   AND r.cancel_requested_at IS NULL
			   AND r.state NOT IN (?, ?, ?)
			   -- Dependencies: every edge must point at a step that is done
			   -- or skipped. An ask_user step parked in waiting_input holds
			   -- its dependents here without a timer (§4.4).
			   AND NOT EXISTS (
				   SELECT 1
				     FROM run_step_deps d
				     JOIN run_steps ds ON ds.id = d.depends_on_step_id
				    WHERE d.step_id = s.id
				      AND ds.state NOT IN (?, ?)
			   )
			 ORDER BY s.available_at, s.created_at
			 LIMIT ?
			 FOR UPDATE OF s SKIP LOCKED
		)
		UPDATE run_steps t
		   SET state            = ?,
		       attempt_count    = t.attempt_count + 1,
		       lease_owner      = ?,
		       lease_expires_at = now() + ` + leaseExpr + `,
		       heartbeat_at     = now(),
		       error_code       = '',
		       error_summary    = '',
		       started_at       = COALESCE(t.started_at, EXTRACT(EPOCH FROM now())::bigint),
		       updated_at       = EXTRACT(EPOCH FROM now())::bigint
		  FROM candidate
		 WHERE t.id = candidate.id
		RETURNING t.*`

		args := []any{types.StepStateQueued}
		args = append(args, kindArgs...)
		args = append(args,
			types.RunStateSucceeded, types.RunStateFailed, types.RunStateCancelled,
			types.StepStateSucceeded, types.StepStateSkipped,
			limit,
			types.StepStateRunning, workerID)
		args = append(args, leaseArgs...)

		var steps []types.RunStep
		if err := tx.Raw(sql, args...).Scan(&steps).Error; err != nil {
			return err
		}
		if len(steps) == 0 {
			return nil
		}

		runIDs := make([]string, 0, len(steps))
		seen := map[string]bool{}
		for _, s := range steps {
			if !seen[s.RunID] {
				seen[s.RunID] = true
				runIDs = append(runIDs, s.RunID)
			}
		}
		var runs []types.Run
		if err := tx.Where("id IN ?", runIDs).Find(&runs).Error; err != nil {
			return err
		}
		byID := map[string]types.Run{}
		for _, r := range runs {
			byID[r.ID] = r
		}

		for _, s := range steps {
			run, found := byID[s.RunID]
			if !found {
				return fmt.Errorf("claim: run %s missing for step %s", s.RunID, s.ID)
			}
			// The execution_id is minted HERE, before dispatch, so an
			// llm_calls row written later can always be tied
			// back to the attempt that made the call (§4.6).
			att := types.StepAttempt{
				ID:          uuid.NewString(),
				TenantID:    s.TenantID,
				RunStepID:   s.ID,
				Number:      s.AttemptCount,
				ExecutionID: uuid.NewString(),
				WorkerID:    workerID,
				State:       types.AttemptStateRunning,
				Retryable:   true,
			}
			if err := tx.Create(&att).Error; err != nil {
				return err
			}
			stepID := s.ID
			if err := emitEvent(tx, &run, &stepID, types.EventStepStarted, map[string]any{
				"step_key": s.StepKey,
				"kind":     s.Kind,
				"label":    s.Label,
				"attempt":  s.AttemptCount,
				"pool":     pool,
			}); err != nil {
				return err
			}
			out = append(out, claimed{step: s, run: run, attempt: att})
		}

		for _, id := range runIDs {
			if err := refreshRunState(tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// kindPredicate builds the pool's kind filter. Named pools claim exactly the
// kinds registered to them; the db pool claims the COMPLEMENT of the other
// pools, so a step whose kind nobody registered is picked up and failed with
// `unknown_step_kind` instead of sitting queued forever.
func kindPredicate(pool string) (string, []any, bool) {
	if pool == types.StepKindDB {
		others := nonDBKinds()
		if len(others) == 0 {
			return "TRUE", nil, true
		}
		return "s.kind NOT IN (" + placeholders(len(others)) + ")", toArgs(others), true
	}
	kinds := kindsForPool(pool)
	if len(kinds) == 0 {
		return "", nil, false
	}
	return "s.kind IN (" + placeholders(len(kinds)) + ")", toArgs(kinds), true
}

// leaseExpression renders the per-kind lease as a CASE, so one claim can cover
// several kinds with different lease budgets without a second round trip.
func leaseExpression(pool string) (string, []any) {
	kinds := kindsForPool(pool)
	if len(kinds) == 0 {
		return "make_interval(secs => ?::double precision)", []any{DefaultLease.Seconds()}
	}
	var b strings.Builder
	args := make([]any, 0, len(kinds)*2+1)
	b.WriteString("make_interval(secs => CASE t.kind")
	for _, k := range kinds {
		b.WriteString(" WHEN ? THEN ?::double precision")
		args = append(args, k, leaseFor(k).Seconds())
	}
	b.WriteString(" ELSE ?::double precision END)")
	args = append(args, DefaultLease.Seconds())
	return b.String(), args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// Enqueue — the write side of the queue
// ---------------------------------------------------------------------------

// RunSpec is a whole plan: the run plus its steps and their edges. Enqueue
// commits all of it in one transaction, so no replica can ever claim a step
// belonging to a half-built plan.
type RunSpec struct {
	TenantID       string
	DossierID      string
	ConversationID *string
	RequestedBy    string

	WorkflowType    string
	WorkflowVersion int
	Instruction     string

	Steps []StepSpec
}

// StepSpec is one unit of work in a plan. DependsOn holds STEP KEYS, which is
// what a workflow definition can name; Enqueue resolves them to ids.
type StepSpec struct {
	// Key is unique within the run and is how the plan monitor and the
	// dependency edges refer to this step.
	Key   string
	Kind  string
	Label string
	Input any

	// DependsOn lists step keys that must reach succeeded or skipped first.
	// Keys may name steps added by an earlier EnqueueSteps call.
	DependsOn []string

	// IdempotencyKey is stable across attempts of the same logical work. Left
	// empty it defaults to "<run_id>:<key>", which is right for a step whose
	// identity is its position in the plan; a step whose side effect is
	// content-addressed should pass its own.
	IdempotencyKey string

	MaxAttempts int
	// Delay postpones availability (a backoff, a scheduled follow-up). The
	// base is the POSTGRES clock read inside the transaction, never the pod's.
	Delay time.Duration
}

// Enqueue commits a whole plan: the run, its steps, its dependency edges and
// the run.created event, in ONE transaction.
func Enqueue(ctx context.Context, spec RunSpec) (types.Run, error) {
	if u.DB == nil {
		return types.Run{}, u.ErrAuthNotConfigured
	}
	if spec.TenantID == "" {
		return types.Run{}, fmt.Errorf("runner.Enqueue: tenant_id is required")
	}
	var run types.Run
	err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		run, err = EnqueueTx(tx, spec)
		return err
	})
	if err != nil {
		return types.Run{}, err
	}

	log.Infof("Run enqueued (run=%s workflow=%s v%d steps=%d)",
		run.ID, run.WorkflowType, run.WorkflowVersion, len(spec.Steps))
	notifyPools()
	return run, nil
}

// EnqueueTx is Enqueue inside a transaction the caller owns, so a run can
// commit atomically with the rows that justify it — an agent turn's messages
// and turn record. Nothing is claimable before the caller commits; call Wake
// afterwards so the pools look at once.
func EnqueueTx(tx *gorm.DB, spec RunSpec) (types.Run, error) {
	if spec.TenantID == "" {
		return types.Run{}, fmt.Errorf("runner.Enqueue: tenant_id is required")
	}
	if spec.WorkflowVersion == 0 {
		spec.WorkflowVersion = 1
	}

	run := types.Run{
		ID:              uuid.NewString(),
		TenantID:        spec.TenantID,
		ConversationID:  spec.ConversationID,
		DossierID:       spec.DossierID,
		RequestedBy:     spec.RequestedBy,
		WorkflowType:    spec.WorkflowType,
		WorkflowVersion: spec.WorkflowVersion,
		Instruction:     spec.Instruction,
		State:           types.RunStatePending,
	}
	if err := tx.Create(&run).Error; err != nil {
		return types.Run{}, err
	}
	if _, err := insertSteps(tx, run, spec.Steps); err != nil {
		return types.Run{}, err
	}
	if err := emitEvent(tx, &run, nil, types.EventRunCreated, map[string]any{
		"workflow_type":    run.WorkflowType,
		"workflow_version": run.WorkflowVersion,
		"steps":            len(spec.Steps),
	}); err != nil {
		return types.Run{}, err
	}
	if len(spec.Steps) > 0 {
		plan, err := planPayload(tx, run.ID)
		if err != nil {
			return types.Run{}, err
		}
		if err := emitEvent(tx, &run, nil, types.EventPlanReady, map[string]any{
			"steps": plan,
		}); err != nil {
			return types.Run{}, err
		}
	}
	return run, refreshRunState(tx, run.ID)
}

// EnqueueSteps adds steps to an existing run — how a workflow grows its own
// fan-out once an earlier step knows how wide it is (one classification step
// per piece, one search step per legal question). Same transaction, same
// dependency resolution, same guarantee.
func EnqueueSteps(ctx context.Context, runID string, specs []StepSpec) ([]types.RunStep, error) {
	if u.DB == nil {
		return nil, u.ErrAuthNotConfigured
	}
	var steps []types.RunStep
	err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run types.Run
		if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
			return err
		}
		if types.TerminalRunState(run.State) {
			return fmt.Errorf("runner.EnqueueSteps: run %s is %s", runID, run.State)
		}
		created, err := insertSteps(tx, run, specs)
		if err != nil {
			return err
		}
		steps = created
		// The WHOLE plan, not just what was added: the monitor replaces its
		// step list from this payload, so a fan-out that sent only its new
		// steps would erase the ones already on screen.
		plan, err := planPayload(tx, run.ID)
		if err != nil {
			return err
		}
		if err := emitEvent(tx, &run, nil, types.EventPlanReady, map[string]any{
			"steps":       plan,
			"steps_added": len(created),
		}); err != nil {
			return err
		}
		return refreshRunState(tx, run.ID)
	})
	if err != nil {
		return nil, err
	}
	log.Infof("Steps enqueued (run=%s added=%d)", runID, len(steps))
	notifyPools()
	return steps, nil
}

// insertSteps writes the step rows and their edges inside an open
// transaction. available_at is derived from a single SELECT now() — the
// Postgres clock — never from this pod's.
// planPayload renders a run's plan for run.plan_ready: every step with the
// dependency edges between them.
//
// It carries the steps THEMSELVES, not a count. The plan monitor draws the
// graph from this payload alone — a thread loads its runs as summaries and
// never fetches the run — so a payload of `{"steps": 8}` left the monitor with
// an empty box under a failed run, which is exactly what it did.
func planPayload(tx *gorm.DB, runID string) ([]map[string]any, error) {
	var steps []types.RunStep
	if err := tx.Where("run_id = ?", runID).Order("created_at, step_key").Find(&steps).Error; err != nil {
		return nil, err
	}
	var deps []types.RunStepDep
	if err := tx.Where("run_id = ?", runID).Find(&deps).Error; err != nil {
		return nil, err
	}
	edges := map[string][]string{}
	for _, d := range deps {
		edges[d.StepID] = append(edges[d.StepID], d.DependsOnStepID)
	}
	out := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		depends := edges[s.ID]
		if depends == nil {
			depends = []string{}
		}
		out = append(out, map[string]any{
			"id":         s.ID,
			"step_key":   s.StepKey,
			"kind":       s.Kind,
			"label":      s.Label,
			"state":      s.State,
			"depends_on": depends,
		})
	}
	return out, nil
}

func insertSteps(tx *gorm.DB, run types.Run, specs []StepSpec) ([]types.RunStep, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	var pgNow time.Time
	if err := tx.Raw("SELECT now()").Scan(&pgNow).Error; err != nil {
		return nil, err
	}

	// Keys already in the run, so a later EnqueueSteps can depend on them.
	byKey := map[string]string{}
	var existing []types.RunStep
	if err := tx.Select("id", "step_key").Where("run_id = ?", run.ID).Find(&existing).Error; err != nil {
		return nil, err
	}
	for _, s := range existing {
		byKey[s.StepKey] = s.ID
	}

	out := make([]types.RunStep, 0, len(specs))
	for _, spec := range specs {
		if spec.Key == "" || spec.Kind == "" {
			return nil, fmt.Errorf("runner: step spec needs a key and a kind")
		}
		var input types.JSONB
		if spec.Input != nil {
			b, err := json.Marshal(spec.Input)
			if err != nil {
				return nil, err
			}
			input = types.JSONB(b)
		}
		idem := spec.IdempotencyKey
		if idem == "" {
			idem = run.ID + ":" + spec.Key
		}
		max := spec.MaxAttempts
		if max <= 0 {
			max = maxAttemptsFor(spec.Kind)
		}
		step := types.RunStep{
			ID:             uuid.NewString(),
			TenantID:       run.TenantID,
			RunID:          run.ID,
			StepKey:        spec.Key,
			Kind:           spec.Kind,
			State:          types.StepStateQueued,
			AvailableAt:    pgNow.Add(spec.Delay),
			MaxAttempts:    max,
			IdempotencyKey: idem,
			Input:          input,
			Label:          spec.Label,
		}
		if err := tx.Create(&step).Error; err != nil {
			return nil, err
		}
		byKey[step.StepKey] = step.ID
		out = append(out, step)
	}

	for i, spec := range specs {
		for _, dep := range spec.DependsOn {
			depID, ok := byKey[dep]
			if !ok {
				return nil, fmt.Errorf("runner: step %q depends on unknown step %q", spec.Key, dep)
			}
			edge := types.RunStepDep{
				ID:              uuid.NewString(),
				RunID:           run.ID,
				StepID:          out[i].ID,
				DependsOnStepID: depID,
			}
			if err := tx.Create(&edge).Error; err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// DependOn adds dependency edges from a step that is ALREADY in the plan onto
// steps a running step has just discovered.
//
// EnqueueSteps can only wire edges for the specs it is inserting, which covers
// a follow-up that depends on its own siblings. It does not cover the other
// direction: a step the ORIGINAL plan already contains that must now wait for
// work the plan could not know about. The ingestion fan-out is exactly that
// case — extract_text discovers a document is a scan and enqueues one ocr_page
// step per page, and the plan's qualify_piece step for that piece must not be
// claimable until they finish, or it reads a text sidecar that does not exist
// yet.
//
// Call it from INSIDE the step that discovered the work: the edges commit
// before that step transitions to succeeded, so the dependent is never
// claimable in the window between the two writes. The insert is ON CONFLICT DO
// NOTHING against uniq (step_id, depends_on_step_id), so a replayed attempt
// converges instead of failing (§4.3). Unknown keys are ignored — a step key
// this run does not contain has nothing to wait for.
func DependOn(ctx context.Context, runID, stepKey string, dependsOnKeys []string) error {
	if u.DB == nil {
		return u.ErrAuthNotConfigured
	}
	if stepKey == "" || len(dependsOnKeys) == 0 {
		return nil
	}
	return u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var steps []types.RunStep
		if err := tx.Select("id", "step_key").
			Where("run_id = ? AND step_key IN ?", runID, append([]string{stepKey}, dependsOnKeys...)).
			Find(&steps).Error; err != nil {
			return err
		}
		byKey := make(map[string]string, len(steps))
		for _, s := range steps {
			byKey[s.StepKey] = s.ID
		}
		stepID, ok := byKey[stepKey]
		if !ok {
			return nil
		}
		edges := make([]types.RunStepDep, 0, len(dependsOnKeys))
		for _, dep := range dependsOnKeys {
			depID, ok := byKey[dep]
			if !ok || depID == stepID {
				continue
			}
			edges = append(edges, types.RunStepDep{
				ID:              uuid.NewString(),
				RunID:           runID,
				StepID:          stepID,
				DependsOnStepID: depID,
			})
		}
		if len(edges) == 0 {
			return nil
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "step_id"}, {Name: "depends_on_step_id"}},
			DoNothing: true,
		}).Create(&edges).Error
	})
}
