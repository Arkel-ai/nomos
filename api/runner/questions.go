package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// HUMAN-IN-THE-LOOP (§4.4).
//
// `ask_user` is an ORDINARY STEP. It writes its questions into run_questions
// and parks itself in waiting_input; its dependents stay queued, held there by
// the same dependency predicate the claimer already applies. There is no
// timer, no in-memory map of pending prompts, and nothing that lives in one
// replica: the pause is a row state, so it survives a pod restart and works
// identically across replicas.
//
// POST /api/runs/:id/answers records the answers, completes the parked step,
// and the claimer picks the workflow back up on its next poll.
//
// A SKIP IS RECORDED AS AN EXPLICIT SKIPPED ANSWER, NEVER AS SILENCE. The run
// must be able to explain what the lawyer was asked and what they chose not to
// decide — "no row" cannot distinguish "not asked" from "declined".

// StepKindAskUser is the built-in clarification step. Workflows enqueue it
// with AskUserInput; nothing else is needed.
const StepKindAskUser = "ask_user"

// ErrWaitingInput parks the step instead of completing or failing it. Return
// it from any step that needs a human before it can go on.
var ErrWaitingInput = errors.New("runner: step is waiting for human input")

// Question is one clarification. Kind is types.QuestionKindSingle / Multi /
// Text.
type Question struct {
	Key     string           `json:"key"`
	Prompt  string           `json:"prompt"`
	Kind    string           `json:"kind"`
	Options []QuestionOption `json:"options,omitempty"`
	// Required marks a question the work cannot finish without
	// (run_questions.required). A skip of it is recorded, never invented.
	Required bool `json:"required,omitempty"`
}

// QuestionOption is one choice in a QCM.
type QuestionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AskUserInput is the jsonb input of a built-in ask_user step.
type AskUserInput struct {
	Questions []Question `json:"questions"`
}

func init() {
	// Clarification is cheap and DB-only: it must never occupy the single
	// OCR or render slot, and it must not wait behind a drafting call.
	Register(StepKindAskUser, askUserStep, RegisterOptions{
		Pool:        types.StepKindDB,
		MaxAttempts: 1,
	})
}

func askUserStep(_ context.Context, sc *StepCtx) (any, error) {
	var in AskUserInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	if len(in.Questions) == 0 {
		return nil, Permanent("no_questions", "an ask_user step was enqueued with no questions")
	}
	if err := sc.Ask(in.Questions); err != nil {
		return nil, err
	}
	return nil, ErrWaitingInput
}

// Ask writes the questions and returns nil; the caller then returns
// ErrWaitingInput. Writing is an upsert on (run_step_id, question_key), so a
// replayed attempt re-asks the same question rather than duplicating it —
// execution is at-least-once, and this is what makes that survivable here.
func (sc *StepCtx) Ask(questions []Question) error {
	if len(questions) == 0 {
		return Permanent("no_questions", "Ask called with no questions")
	}
	rows := make([]types.RunQuestion, 0, len(questions))
	for _, q := range questions {
		if q.Key == "" || q.Prompt == "" {
			return Permanent("invalid_question", "every question needs a key and a prompt")
		}
		kind := q.Kind
		if kind == "" {
			kind = types.QuestionKindSingle
		}
		var opts types.JSONB
		if len(q.Options) > 0 {
			b, err := json.Marshal(q.Options)
			if err != nil {
				return err
			}
			opts = types.JSONB(b)
		}
		rows = append(rows, types.RunQuestion{
			ID:          uuid.NewString(),
			TenantID:    sc.TenantID,
			RunID:       sc.Run.ID,
			RunStepID:   sc.Step.ID,
			QuestionKey: q.Key,
			Prompt:      q.Prompt,
			Kind:        kind,
			Options:     opts,
			State:       types.QuestionStatePending,
			Required:    q.Required,
		})
	}
	return sc.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "run_step_id"}, {Name: "question_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"prompt", "kind", "options", "required"}),
	}).Create(&rows).Error
}

// park moves a claimed step into waiting_input: the lease is released (a
// parked step is not work in flight, so the reaper must not touch it) and the
// attempt is closed as succeeded — asking the question IS what that attempt
// was for. Dependents stay queued.
func park(db *gorm.DB, sc *StepCtx) {
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`
			UPDATE run_steps
			   SET state = ?, lease_owner = '', lease_expires_at = NULL,
			       updated_at = EXTRACT(EPOCH FROM now())::bigint
			 WHERE id = ? AND state = ? AND lease_owner = ? AND attempt_count = ?`,
			types.StepStateWaitingInput, sc.Step.ID, types.StepStateRunning, sc.WorkerID, sc.Step.AttemptCount)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errLeaseLost
		}
		if err := closeAttempt(tx, sc, types.AttemptStateSucceeded, "", true); err != nil {
			return err
		}
		var pending []types.RunQuestion
		if err := tx.Where("run_step_id = ?", sc.Step.ID).Find(&pending).Error; err != nil {
			return err
		}
		// The questions travel WHOLE, not as keys. The event log is what a
		// reconnecting client replays from its cursor, so a payload carrying
		// only keys forces every viewer into a second fetch it has no reason
		// to know it needs — and a client that trusts the payload renders an
		// empty card, which is what happened. Same shape as artifact.ready.
		stepID := sc.Step.ID
		if err := emitEvent(tx, &sc.Run, &stepID, types.EventQuestionAsked, map[string]any{
			"step_key":  sc.Step.StepKey,
			"label":     sc.Step.Label,
			"questions": pending,
		}); err != nil {
			return err
		}
		return refreshRunState(tx, sc.Run.ID)
	})
	if errors.Is(err, errLeaseLost) {
		log.Warnf("Could not park step: lease lost (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
		return
	}
	if err != nil {
		log.Errorf("Parking step failed (run=%s step=%s): %v", sc.Run.ID, sc.Step.ID, err)
		return
	}
	log.Infof("Step parked for input (run=%s step=%s)", sc.Run.ID, sc.Step.ID)
}

// StaleAnswerError refuses an answer to a question of an agent run that is
// already settled, with a value other than the one recorded: the run may
// already have acted on that answer, so it is never overwritten.
type StaleAnswerError struct{ QuestionKey string }

func (e *StaleAnswerError) Error() string {
	return fmt.Sprintf("runner: question %q has already been handled", e.QuestionKey)
}

// AnswerMatches reports whether an answer replays the one recorded on q.
func AnswerMatches(q types.RunQuestion, a types.QuestionAnswer) bool {
	var stored struct {
		Values  []string `json:"values"`
		Text    string   `json:"text"`
		Skipped bool     `json:"skipped"`
	}
	if len(q.Answer) == 0 || json.Unmarshal(q.Answer, &stored) != nil || stored.Skipped != a.Skipped {
		return false
	}
	if a.Skipped {
		return true
	}
	got := append([]string(nil), a.Values...)
	want := append([]string(nil), stored.Values...)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want) && strings.TrimSpace(stored.Text) == strings.TrimSpace(a.Text)
}

// RecordAnswers records the lawyer's replies, completes every parked step
// whose questions are all resolved, and lets the claimer take the workflow
// from there. It is replica-safe by construction: the only state involved is
// rows.
//
// An agent run's question is answered once. Under the run lock, its answer is
// written only while it is still pending: the same answer replayed is a
// no-op, a different one is a *StaleAnswerError — whatever a caller checked
// before taking the lock.
func RecordAnswers(ctx context.Context, tenantID, runID, actor string, answers []types.QuestionAnswer) ([]types.RunQuestion, error) {
	if u.DB == nil {
		return nil, u.ErrAuthNotConfigured
	}
	if len(answers) == 0 {
		return nil, fmt.Errorf("runner: no answers supplied")
	}

	var resolved []types.RunQuestion
	err := u.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The run row lock first, as cancellation takes it: an answer and a
		// cancellation of the same run serialise instead of interleaving.
		var run types.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND tenant_id = ?", runID, tenantID).Take(&run).Error; err != nil {
			return err
		}
		if types.TerminalRunState(run.State) {
			return fmt.Errorf("runner: run %s is %s", runID, run.State)
		}

		var questions []types.RunQuestion
		if err := tx.Where("run_id = ? AND tenant_id = ?", runID, tenantID).
			Find(&questions).Error; err != nil {
			return err
		}
		byKey := map[string]types.RunQuestion{}
		for _, q := range questions {
			byKey[q.QuestionKey] = q
		}

		now := time.Now().Unix()
		touchedSteps := map[string]bool{}
		settleOnce := run.WorkflowType == types.WorkflowNomosAgent
		for _, a := range answers {
			q, ok := byKey[a.QuestionKey]
			if !ok {
				return fmt.Errorf("runner: unknown question %q", a.QuestionKey)
			}
			if settleOnce && q.State != types.QuestionStatePending {
				if !AnswerMatches(q, a) {
					return &StaleAnswerError{QuestionKey: q.QuestionKey}
				}
				resolved = append(resolved, q) // a replay: already recorded
				continue
			}
			// A skip is an ANSWER: an explicit skipped row carrying what was
			// asked, who declined and when. Never an absent row.
			state := types.QuestionStateAnswered
			if a.Skipped {
				state = types.QuestionStateSkipped
			}
			body, err := json.Marshal(map[string]any{
				"values":  a.Values,
				"text":    a.Text,
				"skipped": a.Skipped,
			})
			if err != nil {
				return err
			}
			write := tx.Model(&types.RunQuestion{}).Where("id = ?", q.ID)
			if settleOnce {
				write = write.Where("state = ?", types.QuestionStatePending)
			}
			res := write.Updates(map[string]any{
				"state":       state,
				"answer":      types.JSONB(body),
				"answered_by": actor,
				"answered_at": now,
			})
			if res.Error != nil {
				return res.Error
			}
			if settleOnce && res.RowsAffected == 0 {
				return &StaleAnswerError{QuestionKey: q.QuestionKey}
			}
			q.State, q.Answer, q.AnsweredBy, q.AnsweredAt = state, types.JSONB(body), actor, &now
			resolved = append(resolved, q)
			touchedSteps[q.RunStepID] = true

			stepID := q.RunStepID
			if err := emitEvent(tx, &run, &stepID, types.EventQuestionAnswer, map[string]any{
				"question_key": q.QuestionKey,
				"state":        state,
			}); err != nil {
				return err
			}
		}

		for stepID := range touchedSteps {
			var pending int64
			if err := tx.Model(&types.RunQuestion{}).
				Where("run_step_id = ? AND state = ?", stepID, types.QuestionStatePending).
				Count(&pending).Error; err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			var stepQs []types.RunQuestion
			if err := tx.Where("run_step_id = ?", stepID).Find(&stepQs).Error; err != nil {
				return err
			}
			out := map[string]any{}
			for _, q := range stepQs {
				out[q.QuestionKey] = json.RawMessage(q.Answer)
			}
			body, err := json.Marshal(map[string]any{"answers": out})
			if err != nil {
				return err
			}
			// Completing the parked step is a conditional transition on
			// waiting_input, so two lawyers answering at once cannot
			// complete it twice.
			res := tx.Exec(`
				UPDATE run_steps
				   SET state = ?, output = ?,
				       finished_at = EXTRACT(EPOCH FROM now())::bigint,
				       updated_at  = EXTRACT(EPOCH FROM now())::bigint
				 WHERE id = ? AND state = ?`,
				types.StepStateSucceeded, types.JSONB(body), stepID, types.StepStateWaitingInput)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				continue
			}
			sid := stepID
			if err := emitEvent(tx, &run, &sid, types.EventStepSucceeded, map[string]any{
				"kind":  StepKindAskUser,
				"state": types.StepStateSucceeded,
			}); err != nil {
				return err
			}
		}
		return refreshRunState(tx, runID)
	})
	if err != nil {
		return nil, err
	}
	log.Infof("Answers recorded (run=%s answers=%d)", runID, len(answers))
	notifyPools()
	return resolved, nil
}
