package runner

import (
	"encoding/json"
	"sync"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
)

// run_events is the user-visible progress log and the cursor the thread
// replays from (§4.5, §4.5b). Two rules make the cursor trustworthy:
//
//  1. an event is inserted in the SAME TRANSACTION as the state change it
//     describes — never afterwards, never best-effort;
//  2. the transport is not the source of truth. Polling and SSE both read
//     these rows by durable id, so a pod restart costs the view, never the log.
//
// This is progress, not evidence: audit_events is the compliance log. Never
// merge them, and put no prompt, document text or presigned URL in either
// (§7.4).

func emitEvent(tx *gorm.DB, run *types.Run, stepID *string, evType string, payload any) error {
	var body types.JSONB
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = types.JSONB(b)
	}
	runID := run.ID
	ev := types.RunEvent{
		TenantID:       run.TenantID,
		RunID:          &runID,
		ConversationID: run.ConversationID,
		RunStepID:      stepID,
		Type:           evType,
		Payload:        body,
	}
	return tx.Create(&ev).Error
}

// refreshRunState rolls the step states up into the run state and emits
// run.state_changed when it actually moves. It must be called inside the same
// transaction as the step transition that triggered it.
//
// A terminal run is never revived: cancelled/failed/succeeded are final, so a
// step that lands after a cancellation cannot resurrect the run.
func refreshRunState(tx *gorm.DB, runID string) error {
	var run types.Run
	if err := tx.Where("id = ?", runID).Take(&run).Error; err != nil {
		return err
	}
	if types.TerminalRunState(run.State) {
		return nil
	}

	type row struct {
		State string
		N     int64
	}
	var rows []row
	if err := tx.Model(&types.RunStep{}).
		Select("state, count(*) as n").
		Where("run_id = ?", runID).
		Group("state").Scan(&rows).Error; err != nil {
		return err
	}

	counts := map[string]int64{}
	var total, terminal int64
	for _, r := range rows {
		counts[r.State] = r.N
		total += r.N
		if types.TerminalStepState(r.State) {
			terminal += r.N
		}
	}

	next := run.State
	switch {
	case total == 0:
		next = types.RunStatePending
	case counts[types.StepStateFailed] > 0:
		// A step that exhausted its attempts fails the run immediately: the
		// claimer excludes terminal runs, so nothing else is picked up.
		next = types.RunStateFailed
	case terminal == total:
		next = types.RunStateSucceeded
	case counts[types.StepStateRunning] > 0:
		next = types.RunStateRunning
	case counts[types.StepStateWaitingInput] > 0:
		next = types.RunStateWaitingInput
	default:
		next = types.RunStateRunning
	}

	if next == run.State {
		return nil
	}

	updates := map[string]any{
		"state":      next,
		"updated_at": gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
	}
	if run.StartedAt == nil && next != types.RunStatePending {
		updates["started_at"] = gorm.Expr("EXTRACT(EPOCH FROM now())::bigint")
	}
	if types.TerminalRunState(next) {
		updates["finished_at"] = gorm.Expr("EXTRACT(EPOCH FROM now())::bigint")
	}
	if next == types.RunStateFailed && run.FailureCode == "" {
		// The failing step's own code, so "unsupported_request" and
		// "invalid_model_output" stay distinguishable from a crash.
		code := "step_failed"
		var failed types.RunStep
		if err := tx.Select("error_code").Where("run_id = ? AND state = ? AND error_code <> ''",
			runID, types.StepStateFailed).Order("finished_at").Take(&failed).Error; err == nil {
			code = failed.ErrorCode
		}
		updates["failure_code"] = code
	}
	if err := tx.Model(&types.Run{}).Where("id = ? AND state = ?", runID, run.State).
		Updates(updates).Error; err != nil {
		return err
	}

	run.State = next
	payload := map[string]any{
		"state":          next,
		"steps_total":    total,
		"steps_terminal": terminal,
	}
	if code, ok := updates["failure_code"]; ok {
		payload["failure_code"] = code
		run.FailureCode, _ = code.(string)
	}
	if err := emitEvent(tx, &run, nil, types.EventRunStateChanged, payload); err != nil {
		return err
	}
	if types.TerminalRunState(next) {
		runTerminalHook(tx, run)
	}
	return nil
}

// closeRun ends a live run in state from inside a step's publication (see
// Terminal): steps still queued or parked are cancelled, the run carries
// code, run.state_changed is emitted, then the terminal hook runs — all in the
// caller's transaction. Running siblings finish on their own and, finding the
// run closed, publish nothing.
func closeRun(tx *gorm.DB, run types.Run, state, code string) error {
	if err := tx.Exec(`
		UPDATE run_steps
		   SET state = ?, error_code = 'run_closed',
		       lease_owner = '', lease_expires_at = NULL,
		       finished_at = EXTRACT(EPOCH FROM now())::bigint,
		       updated_at  = EXTRACT(EPOCH FROM now())::bigint
		 WHERE run_id = ? AND state IN (?, ?)`,
		types.StepStateCancelled, run.ID, types.StepStateQueued, types.StepStateWaitingInput).Error; err != nil {
		return err
	}
	updates := map[string]any{
		"state":       state,
		"finished_at": gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
		"updated_at":  gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
	}
	if run.StartedAt == nil {
		updates["started_at"] = gorm.Expr("EXTRACT(EPOCH FROM now())::bigint")
	}
	if state != types.RunStateSucceeded && code != "" {
		updates["failure_code"] = code
		run.FailureCode = code
	}
	if err := tx.Model(&types.Run{}).Where("id = ?", run.ID).Updates(updates).Error; err != nil {
		return err
	}
	run.State = state
	payload := map[string]any{"state": state}
	if run.FailureCode != "" && state != types.RunStateSucceeded {
		payload["failure_code"] = run.FailureCode
	}
	if err := emitEvent(tx, &run, nil, types.EventRunStateChanged, payload); err != nil {
		return err
	}
	runTerminalHook(tx, run)
	return nil
}

// RunTerminalHook runs inside the transaction that moves a run of its
// workflow type into a terminal state — whichever path got it there: the last
// step finishing, a step exhausting its attempts, the reaper giving up,
// a cancellation, or a Terminal step. It is how a run that ends without its
// own explanation (a crashed step, a cancelled turn) still closes what the
// lawyer is looking at, in the same commit.
type RunTerminalHook func(tx *gorm.DB, run types.Run) error

var (
	hookMu        sync.RWMutex
	terminalHooks = map[string]RunTerminalHook{}
)

// OnRunTerminal installs the terminal hook of one workflow type. Call it from
// an init(); a second registration for the same type replaces the first.
func OnRunTerminal(workflowType string, fn RunTerminalHook) {
	hookMu.Lock()
	defer hookMu.Unlock()
	terminalHooks[workflowType] = fn
}

// runTerminalHook calls the run's hook inside a savepoint. A failing hook is
// logged and rolled back to the savepoint — never allowed to abort the
// transition that called it, or a step could not even fail.
func runTerminalHook(tx *gorm.DB, run types.Run) {
	hookMu.RLock()
	fn := terminalHooks[run.WorkflowType]
	hookMu.RUnlock()
	if fn == nil {
		return
	}
	if err := tx.SavePoint("run_terminal_hook").Error; err != nil {
		log.Errorf("Terminal hook skipped: savepoint failed (run=%s): %v", run.ID, err)
		return
	}
	if err := fn(tx, run); err != nil {
		log.Errorf("Terminal hook failed (run=%s state=%s): %v", run.ID, run.State, err)
		if rbErr := tx.RollbackTo("run_terminal_hook").Error; rbErr != nil {
			log.Errorf("Terminal hook rollback failed (run=%s): %v", run.ID, rbErr)
		}
	}
}
