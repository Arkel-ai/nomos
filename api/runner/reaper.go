package runner

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/types"
)

// THE REAPER.
//
// The pod can die mid-mission: main.go allows 10 s of graceful shutdown, a
// drafting step takes 60–180 s, Kubernetes' 30 s termination window does not
// save us either, and neither survives node loss (§2.1). Leases plus idempotent
// publication are the ONLY real recovery mechanism, and this loop is the half
// that notices.
//
// Two properties matter more than anything else here:
//
//   - It requeues only AFTER expiry, measured against the POSTGRES clock. A
//     pod whose clock drifts forward would otherwise steal live work.
//   - The requeue is a CONDITIONAL UPDATE on the OLD lease owner. The
//     candidate rows are read first, then each is updated
//     WHERE id = ? AND lease_owner = <the owner we saw>
//     AND attempt_count = <the attempt we saw> AND state = 'running'
//     AND lease_expires_at < now(). A worker that is slow but alive
//     heartbeats between the read and the write, pushing lease_expires_at into
//     the future, and its row no longer matches — so it is never
//     double-claimed.
//
// Attempts are capped. An exhausted step goes to state=failed with an
// error_code; there is no infinite retry loop, and "retry from this step" is a
// new attempt with an audit trail, not a silent reset (§4.3).

func reaperLoop(ctx context.Context, db *gorm.DB) {
	log.Debugf("Reaper started (interval=%s)", ReaperInterval)
	t := time.NewTicker(ReaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Debugf("Reaper stopped")
			return
		case <-t.C:
			n, err := ReapExpired(ctx, db)
			if err != nil && ctx.Err() == nil {
				log.Errorf("Reaper sweep failed: %v", err)
			}
			if n > 0 {
				log.Warnf("Reaper reclaimed %d expired lease(s)", n)
				notifyPools()
			}
		}
	}
}

// ReapExpired sweeps once and returns how many leases it reclaimed. Exported
// so the tests can drive it deterministically instead of waiting on a ticker.
func ReapExpired(ctx context.Context, db *gorm.DB) (int, error) {
	var candidates []types.RunStep
	if err := db.WithContext(ctx).
		Where("state = ? AND lease_expires_at IS NOT NULL AND lease_expires_at < now()",
			types.StepStateRunning).
		Limit(ClaimBatch).
		Find(&candidates).Error; err != nil {
		return 0, err
	}

	reclaimed := 0
	for _, step := range candidates {
		exhausted := step.AttemptCount >= step.MaxAttempts
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var res *gorm.DB
			if exhausted {
				res = tx.Exec(`
					UPDATE run_steps
					   SET state = ?, error_code = 'lease_expired',
					       error_summary = 'the worker holding this step disappeared and no attempts remain',
					       lease_owner = '', lease_expires_at = NULL,
					       finished_at = EXTRACT(EPOCH FROM now())::bigint,
					       updated_at  = EXTRACT(EPOCH FROM now())::bigint
					 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?
					   AND lease_expires_at IS NOT NULL AND lease_expires_at < now()`,
					types.StepStateFailed, step.ID, types.StepStateRunning, step.LeaseOwner, step.AttemptCount)
			} else {
				res = tx.Exec(`
					UPDATE run_steps
					   SET state = ?, error_code = 'lease_expired',
					       error_summary = 'the worker holding this step disappeared; requeued',
					       lease_owner = '', lease_expires_at = NULL,
					       available_at = now(),
					       updated_at = EXTRACT(EPOCH FROM now())::bigint
					 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?
					   AND lease_expires_at IS NOT NULL AND lease_expires_at < now()`,
					types.StepStateQueued, step.ID, types.StepStateRunning, step.LeaseOwner, step.AttemptCount)
			}
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				// The owner heartbeated (or another reaper got there
				// first). Leave it alone — this is exactly the
				// double-claim the conditional update exists to prevent.
				return errLeaseLost
			}

			// The abandoned attempt is closed, not deleted: retry history is
			// what makes a failure diagnosable (§9.1(5)).
			if err := tx.Exec(`
				UPDATE step_attempts
				   SET state = ?, retryable = ?, error_code = 'lease_expired',
				       finished_at = EXTRACT(EPOCH FROM now())::bigint
				 WHERE run_step_id = ? AND state = ?`,
				types.AttemptStateAbandoned, !exhausted, step.ID, types.AttemptStateRunning).Error; err != nil {
				return err
			}

			var run types.Run
			if err := tx.Where("id = ?", step.RunID).Take(&run).Error; err != nil {
				return err
			}
			evType := types.EventStepRetrying
			if exhausted {
				evType = types.EventStepFailed
			}
			stepID := step.ID
			if err := emitEvent(tx, &run, &stepID, evType, map[string]any{
				"step_key":   step.StepKey,
				"kind":       step.Kind,
				"label":      step.Label,
				"attempt":    step.AttemptCount,
				"error_code": "lease_expired",
				"retrying":   !exhausted,
			}); err != nil {
				return err
			}
			return refreshRunState(tx, step.RunID)
		})
		switch {
		case errors.Is(err, errLeaseLost):
			log.Debugf("Reaper skipped step %s: lease was renewed or already reclaimed", step.ID)
		case err != nil:
			log.Errorf("Reaper failed to reclaim step %s: %v", step.ID, err)
		default:
			reclaimed++
			log.Warnf("Lease expired (run=%s step=%s kind=%s attempt=%d/%d exhausted=%t)",
				step.RunID, step.ID, step.Kind, step.AttemptCount, step.MaxAttempts, exhausted)
		}
	}
	return reclaimed, nil
}
