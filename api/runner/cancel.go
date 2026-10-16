package runner

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Cancellation is a DURABLE FLAG (runs.cancel_requested_at), not a
// context.CancelFunc living in one replica.
//
// A CancelFunc only reaches the goroutine that holds it. With replicas > 1 and
// no worker/API split, the request that cancels a run is very likely served by
// a pod that is not running the step — so the flag has to be in Postgres,
// where every replica can see it. It is read in three places:
//
//	before the claim   → claim.go excludes runs with the flag set
//	at each heartbeat  → StepCtx.Heartbeat sets sc.cancelled and the step's
//	                     context is cancelled, aborting the external call
//	before publishing  → finishSuccess re-reads it, so a run the lawyer
//	                     stopped does not silently gain a new output
//
// What cancellation NEVER does is erase evidence: step_attempts, llm_calls
// and any artifact already published stay exactly as they are. The
// mission stops; the record of what it did and what it cost does not.

// RequestCancel stamps the flag, cancels the steps that have not started, and
// leaves running steps to observe it at their next safe point. Nothing is
// killed mid-write.
func RequestCancel(ctx context.Context, tenantID, runID, actor string) (types.Run, error) {
	if u.DB == nil {
		return types.Run{}, u.ErrAuthNotConfigured
	}
	var run types.Run
	err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The run row lock first: the order every step publication and fenced
		// write takes, so a cancellation and a commit can never interleave.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", runID, tenantID).Take(&run).Error; err != nil {
			return err
		}
		if types.TerminalRunState(run.State) {
			return nil
		}

		now := time.Now().Unix()
		if err := tx.Model(&types.Run{}).
			Where("id = ? AND cancel_requested_at IS NULL", runID).
			Updates(map[string]any{
				"cancel_requested_at": gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
			}).Error; err != nil {
			return err
		}

		// Queued and parked steps stop existing as work. A RUNNING step is
		// deliberately left alone: it holds a lease and is mid-flight, and
		// killing its row here would let the reaper hand a cancelled run's
		// step to another worker.
		if err := tx.Exec(`
			UPDATE run_steps
			   SET state = ?, error_code = 'cancelled',
			       lease_owner = '', lease_expires_at = NULL,
			       finished_at = EXTRACT(EPOCH FROM now())::bigint,
			       updated_at  = EXTRACT(EPOCH FROM now())::bigint
			 WHERE run_id = ? AND state IN (?, ?)`,
			types.StepStateCancelled, runID,
			types.StepStateQueued, types.StepStateWaitingInput).Error; err != nil {
			return err
		}

		if err := tx.Model(&types.Run{}).Where("id = ?", runID).
			Updates(map[string]any{
				"state":        types.RunStateCancelled,
				"failure_code": "cancelled_by_user",
				"finished_at":  gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
				"updated_at":   gorm.Expr("EXTRACT(EPOCH FROM now())::bigint"),
			}).Error; err != nil {
			return err
		}

		run.State = types.RunStateCancelled
		run.FailureCode = "cancelled_by_user"
		run.CancelRequestedAt = &now
		if err := emitEvent(tx, &run, nil, types.EventRunStateChanged, map[string]any{
			"state":  types.RunStateCancelled,
			"reason": "cancelled_by_user",
			"actor":  actor,
		}); err != nil {
			return err
		}
		runTerminalHook(tx, run)
		return nil
	})
	if err != nil {
		return types.Run{}, err
	}
	log.Infof("Run cancellation requested (run=%s tenant=%s)", runID, tenantID)
	return run, nil
}

// cancelRequested reads the durable flag. Errors are reported to the caller,
// which treats "unknown" as "not cancelled" — a failed read must not silently
// abort work in flight.
func cancelRequested(db *gorm.DB, runID string) (bool, error) {
	var n int64
	err := db.Model(&types.Run{}).
		Where("id = ? AND cancel_requested_at IS NOT NULL", runID).
		Count(&n).Error
	return n > 0, err
}
