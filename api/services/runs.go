package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var runsLog = u.NewLogger("services.runs")

// Runs — the mission surface (docs/PLAN.md §4, §10). These endpoints only ever
// read and write rows; the runner goroutines (package runner) do the work.
//
// Every read below goes through the tenant-scoped repository. `WHERE tenant_id
// = ?` is not RLS and one missed clause is a cross-tenant breach (§6.1), so
// there is no ad-hoc u.DB query against a run, step, event or question here.

// runsLoad fetches a run through the tenant repository and authorizes the
// caller against the dossier it operates on. A run belonging to another tenant
// is reported as 404, never 403 — the two must be indistinguishable.
func runsLoad(c *gin.Context, id, act string) (*u.TenantRepo, *types.Run, *u.AuthUser, int, any) {
	repo, status, body := tenantRepo(c)
	if repo == nil {
		return nil, nil, nil, status, body
	}
	user := middlewares.CurrentUser(c)
	if user == nil {
		return nil, nil, nil, http.StatusUnauthorized, gin.H{"detail": "Not authenticated"}
	}

	var run types.Run
	err := repo.Runs().Where("id = ?", id).Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil, http.StatusNotFound, gin.H{"detail": "Run not found"}
	}
	if err != nil {
		runsLog.Errorf("Run lookup failed (run=%s): %v", id, err)
		return nil, nil, nil, http.StatusInternalServerError, gin.H{"detail": "Run lookup failed"}
	}

	if !runsMayAccess(c, repo, run, act) {
		runsLog.Debugf("Run access denied (run=%s user=%s act=%s)", id, user.ID, act)
		return nil, nil, nil, http.StatusNotFound, gin.H{"detail": "Run not found"}
	}
	return repo, &run, user, 0, nil
}

// runsMayAccess authorizes a run through its dossier: a run is never more
// accessible than the collection it reorganises. The dossier itself was loaded
// tenant-scoped, so this is authorization, not isolation.
func runsMayAccess(c *gin.Context, repo *u.TenantRepo, run types.Run, act string) bool {
	user := middlewares.CurrentUser(c)
	if user == nil {
		return false
	}
	if run.RequestedBy == user.ID {
		return true
	}
	if run.DossierID != "" {
		var d types.Dossier
		if err := repo.Dossiers().Where("id = ?", run.DossierID).Take(&d).Error; err == nil {
			return canAccessDossier(c, repo, d, act)
		}
	}
	ok, err := u.Enforce(user.ID, repo.TenantID(), types.ObjectDossier, act)
	return err == nil && ok
}

// runsCursor is the newest run_events.id visible for a run — where the client
// starts polling from.
func runsCursor(repo *u.TenantRepo, runID string) uint64 {
	var cursor *uint64
	if err := repo.RunEvents().Where("run_id = ?", runID).
		Select("MAX(id)").Scan(&cursor).Error; err != nil || cursor == nil {
		return 0
	}
	return *cursor
}

// GetRun returns the plan the monitor renders: steps with their dependency
// edges (so parallel fan-out draws as parallel), attempt counts (so a retried
// step shows it), open questions and published artifacts.
func GetRun(c *gin.Context, id string) (int, any) {
	repo, run, _, status, body := runsLoad(c, id, types.ActionRead)
	if run == nil {
		return status, body
	}

	detail := types.RunDetail{Run: *run}
	if err := repo.RunSteps().Where("run_id = ?", run.ID).
		Order("created_at, step_key").Find(&detail.Steps).Error; err != nil {
		runsLog.Errorf("Step listing failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Run lookup failed"}
	}
	if run.WorkflowType == types.WorkflowNomosAgent {
		// An agent step's input and output hold raw tool arguments, private
		// observations and the model's own text: they stay server-side. The
		// thread shows the agent through its activity rows instead.
		for i := range detail.Steps {
			detail.Steps[i].Input, detail.Steps[i].Output = nil, nil
		}
	}
	if err := repo.RunStepDeps([]string{run.ID}).Find(&detail.Deps).Error; err != nil {
		runsLog.Errorf("Dep listing failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Run lookup failed"}
	}
	if err := repo.RunQuestions().Where("run_id = ?", run.ID).
		Order("asked_at").Find(&detail.Questions).Error; err != nil {
		return http.StatusInternalServerError, gin.H{"detail": "Run lookup failed"}
	}
	if err := repo.Artifacts().Where("run_id = ?", run.ID).
		Order("created_at").Find(&detail.Artifacts).Error; err != nil {
		return http.StatusInternalServerError, gin.H{"detail": "Run lookup failed"}
	}
	detail.Cursor = runsCursor(repo, run.ID)

	runsLog.Debugf("Run detail served (run=%s steps=%d questions=%d)",
		run.ID, len(detail.Steps), len(detail.Questions))
	return http.StatusOK, detail
}

// RunEvents is the cursor poll: WHERE tenant_id = ? AND run_id = ? AND id >
// after ORDER BY id LIMIT n, polled every 1–2 s (§4.5). Dull, durable,
// trivially authorizable, resumable across a pod restart. run_events is the
// source of truth; the transport is not.
//
// This is deliberately NOT SSE. A token stream severed by a pod restart is not
// resumable and its usage trailer may never arrive; a cursor is.
func RunEvents(c *gin.Context, id string, q types.EventsQuery) (int, any) {
	repo, run, _, status, body := runsLoad(c, id, types.ActionRead)
	if run == nil {
		return status, body
	}
	after, limit := q.Normalized()

	var events []types.RunEvent
	if err := repo.RunEvents().Where("run_id = ? AND id > ?", run.ID, after).
		Order("id").Limit(limit).Find(&events).Error; err != nil {
		runsLog.Errorf("Event poll failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Event lookup failed"}
	}

	next := after
	if n := len(events); n > 0 {
		next = events[n-1].ID
	}
	return http.StatusOK, types.EventsResponse{Events: events, NextAfter: next}
}

// AnswerRun records the human-in-the-loop answers, completes the parked
// `ask_user` step and lets the claimer pick the workflow back up (§4.4). A
// SKIP IS RECORDED AS AN ANSWER, never as silence: the run must be able to
// explain what the lawyer was asked and what they chose not to decide.
func AnswerRun(c *gin.Context, id string, body types.AnswersRequest) (int, any) {
	repo, run, user, status, errBody := runsLoad(c, id, types.ActionWrite)
	if run == nil {
		return status, errBody
	}
	if len(body.Answers) == 0 {
		return http.StatusBadRequest, gin.H{"detail": "No answers supplied"}
	}

	answers := body.Answers
	if run.WorkflowType == types.WorkflowNomosAgent {
		var status int
		var errBody any
		answers, status, errBody = validateAgentAnswers(repo, run.ID, body.Answers)
		if status != 0 {
			return status, errBody
		}
		if len(answers) == 0 {
			// Every answer replays one already recorded: idempotent.
			var current []types.RunQuestion
			if err := repo.RunQuestions().Where("run_id = ?", run.ID).Order("asked_at").Find(&current).Error; err != nil {
				return http.StatusInternalServerError, gin.H{"detail": "Question lookup failed"}
			}
			runsLog.Infof("Answers replayed, nothing to record (run=%s answers=%d)", run.ID, len(body.Answers))
			return http.StatusOK, gin.H{"questions": current}
		}
	}

	questions, err := runner.RecordAnswers(c.Request.Context(), repo.TenantID(), run.ID, user.ID, answers)
	var stale *runner.StaleAnswerError
	if errors.As(err, &stale) {
		// Another answer settled the question after this one was validated.
		runsLog.Infof("Stale answer refused at record time (run=%s question=%s)", run.ID, stale.QuestionKey)
		return http.StatusConflict, gin.H{"detail": "This question has already been handled", "code": "stale_question"}
	}
	if err != nil {
		runsLog.Warnf("Recording answers failed (run=%s answers=%d): %v", run.ID, len(body.Answers), err)
		return http.StatusBadRequest, gin.H{"detail": err.Error()}
	}

	runsLog.Infof("Answers recorded (run=%s answers=%d)", run.ID, len(questions))
	return http.StatusOK, gin.H{"questions": questions}
}

// CancelRun stamps cancel_requested_at. Cancellation is a REQUEST observed by
// a running step at its next safe point, not a kill — nothing is aborted
// mid-write, and queued steps simply stop being claimed. Completed evidence
// and cost records are never erased.
func CancelRun(c *gin.Context, id string) (int, any) {
	repo, run, user, status, errBody := runsLoad(c, id, types.ActionWrite)
	if run == nil {
		return status, errBody
	}

	updated, err := runner.RequestCancel(c.Request.Context(), repo.TenantID(), run.ID, user.ID)
	if err != nil {
		runsLog.Errorf("Cancel failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Cancel failed"}
	}

	var at int64
	if updated.CancelRequestedAt != nil {
		at = *updated.CancelRequestedAt
	}
	return http.StatusOK, types.CancelResponse{
		RunID:             updated.ID,
		State:             updated.State,
		CancelRequestedAt: at,
	}
}

// RunArtifacts lists the deliverables a run published. An artifact row exists
// only after its bytes were written and validated, so anything listed here is
// downloadable (§4.3).
func RunArtifacts(c *gin.Context, id string) (int, any) {
	repo, run, _, status, body := runsLoad(c, id, types.ActionRead)
	if run == nil {
		return status, body
	}
	var artifacts []types.Artifact
	if err := repo.Artifacts().Where("run_id = ?", run.ID).
		Order("created_at").Find(&artifacts).Error; err != nil {
		runsLog.Errorf("Artifact listing failed (run=%s): %v", run.ID, err)
		return http.StatusInternalServerError, gin.H{"detail": "Artifact lookup failed"}
	}
	return http.StatusOK, gin.H{"artifacts": artifacts, "total": len(artifacts)}
}

// validateAgentAnswers checks answers to an agent's questions before they
// release paid work (§5.4): the question must belong to this run (404, which
// discloses nothing), still be pending — the same answer replayed is
// idempotent and dropped, a changed one is 409 — and fit its kind (422). A
// skip is always accepted and recorded; it is never filled in.
func validateAgentAnswers(repo *u.TenantRepo, runID string, answers []types.QuestionAnswer) ([]types.QuestionAnswer, int, any) {
	var questions []types.RunQuestion
	if err := repo.RunQuestions().Where("run_id = ?", runID).Find(&questions).Error; err != nil {
		runsLog.Errorf("Question lookup failed (run=%s): %v", runID, err)
		return nil, http.StatusInternalServerError, gin.H{"detail": "Question lookup failed"}
	}
	byKey := make(map[string]types.RunQuestion, len(questions))
	for _, q := range questions {
		byKey[q.QuestionKey] = q
	}
	seen := map[string]bool{}
	out := make([]types.QuestionAnswer, 0, len(answers))
	for _, a := range answers {
		key := strings.TrimSpace(a.QuestionKey)
		q, ok := byKey[key]
		if !ok {
			return nil, http.StatusNotFound, gin.H{"detail": "Question not found"}
		}
		if seen[key] {
			return nil, http.StatusUnprocessableEntity, gin.H{"detail": "A question is answered twice", "code": "invalid_answer"}
		}
		seen[key] = true
		if q.State != types.QuestionStatePending {
			if runner.AnswerMatches(q, a) {
				continue
			}
			runsLog.Infof("Stale answer refused (run=%s question=%s state=%s)", runID, key, q.State)
			return nil, http.StatusConflict, gin.H{"detail": "This question has already been handled", "code": "stale_question"}
		}
		if !a.Skipped {
			if detail := agentAnswerProblem(q, a); detail != "" {
				return nil, http.StatusUnprocessableEntity, gin.H{"detail": detail, "code": "invalid_answer"}
			}
		}
		a.QuestionKey = key
		out = append(out, a)
	}
	return out, 0, nil
}

// maxAgentAnswerRunes bounds a free-text answer that goes back to the model.
const maxAgentAnswerRunes = 4000

func agentAnswerProblem(q types.RunQuestion, a types.QuestionAnswer) string {
	if utf8.RuneCountInString(a.Text) > maxAgentAnswerRunes {
		return fmt.Sprintf("The answer exceeds %d characters", maxAgentAnswerRunes)
	}
	var options []runner.QuestionOption
	if len(q.Options) > 0 {
		_ = json.Unmarshal(q.Options, &options)
	}
	allowed := make(map[string]bool, len(options))
	for _, o := range options {
		allowed[o.Value] = true
	}
	switch q.Kind {
	case types.QuestionKindText:
		if strings.TrimSpace(a.Text) == "" || len(a.Values) > 0 {
			return "This question expects a text answer"
		}
	case types.QuestionKindMulti:
		if len(a.Values) == 0 {
			return "Choose at least one option"
		}
		picked := map[string]bool{}
		for _, v := range a.Values {
			if !allowed[v] || picked[v] {
				return "Unknown or repeated option"
			}
			picked[v] = true
		}
	default:
		if len(a.Values) != 1 || !allowed[a.Values[0]] {
			return "Choose exactly one of the proposed options"
		}
	}
	return ""
}
