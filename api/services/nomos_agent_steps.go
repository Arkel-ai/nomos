package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The agent's runner steps (§3.3–3.5, §5.1, §5.4). Each step reads the turn
// and its own durable records, does at most one bounded piece of external
// work, and commits what it accepted together with the steps that follow —
// so a process death at any point resumes, and never repeats an accepted
// decision, tool result, question or publication.

func init() {
	runner.Register(stepAgentBootstrap, agentBootstrapStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	runner.Register(stepAgentWait, agentWaitStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	// Every attempt of a decision reserves a new dispatch: the attempt cap
	// bounds transport retries, the dispatch ceiling bounds everything.
	runner.Register(stepAgentDecide, agentDecideStep, runner.RegisterOptions{Pool: types.StepKindLLM, MaxAttempts: 4})
	runner.Register(stepAgentToolDB, agentToolStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	runner.Register(stepAgentToolExt, agentToolStep, runner.RegisterOptions{Pool: types.StepKindLLM, MaxAttempts: 3})
	runner.Register(stepAgentToolRender, agentToolStep, runner.RegisterOptions{Pool: types.StepKindRender, MaxAttempts: 3})
	runner.Register(stepAgentObserve, agentObserveStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	runner.Register(stepAgentGate, agentGateStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 3})
	runner.Register(stepAgentPublish, agentPublishStep, runner.RegisterOptions{Pool: types.StepKindRender, MaxAttempts: 3})
	runner.Register(stepAgentDeliver, agentDeliverStep, runner.RegisterOptions{Pool: types.StepKindDB, MaxAttempts: 5})
	runner.OnRunTerminal(types.WorkflowNomosAgent, agentRunTerminalHook)
}

// errAgentCapReached is the dispatch reservation finding the ceiling.
var errAgentCapReached = errors.New("agent dispatch ceiling reached")

// agentEnd ends the turn from inside a step. In one commit the turn records
// its outcome, the run closes failed with code, and the run's terminal hook
// writes the server's explanation into the thread. No model is called to
// describe the failure.
func agentEnd(sc *runner.StepCtx, output any, outcome, code string, extra func(tx *u.TenantRepo) error) runner.Terminal {
	if code == "" {
		code = outcome
	}
	nomosAgentLog.Warnf("Agent turn ending (run=%s outcome=%s code=%s step=%s)", sc.Run.ID, outcome, code, sc.Step.StepKey)
	return runner.Terminal{
		Output:      output,
		RunState:    types.RunStateFailed,
		FailureCode: code,
		Commit: func(tx *gorm.DB) error {
			repo := u.ForTenantTx(tx, sc.TenantID)
			if extra != nil {
				if err := extra(repo); err != nil {
					return err
				}
			}
			now := time.Now().Unix()
			return repo.AgentTurns().Where("run_id = ? AND outcome = ''", sc.Run.ID).
				Updates(map[string]any{"outcome": outcome, "finished_at": now, "updated_at": now}).Error
		},
	}
}

// agentCommit wraps repository work into an Expansion commit.
func agentCommit(tenantID string, fns ...func(tx *u.TenantRepo) error) func(tx *gorm.DB) error {
	return func(tx *gorm.DB) error {
		repo := u.ForTenantTx(tx, tenantID)
		for _, fn := range fns {
			if fn == nil {
				continue
			}
			if err := fn(repo); err != nil {
				return err
			}
		}
		return nil
	}
}

func agentNextDecision(n int, after string) runner.StepSpec {
	return runner.StepSpec{
		Key: agentDecisionKey(n), Kind: stepAgentDecide, Label: fmt.Sprintf("Décision %d", n),
		Input: agentDecideInput{Decision: n}, DependsOn: []string{after},
	}
}

// ---------------------------------------------------------------------------
// Bootstrap and the durable wait for attachments
// ---------------------------------------------------------------------------

type agentWaitInput struct {
	N      int      `json:"n"`
	Since  int64    `json:"since"`
	Pieces []string `json:"pieces"`
}

func agentBootstrapStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	if AgentTools == nil {
		return agentEnd(sc, nil, types.AgentOutcomeFailed, agentCodeRuntimeUnavailable, nil), nil
	}
	pieces := agentTurnAttachments(repo, turn)
	reading := attachmentsBeingRead(repo, pieces)
	if len(reading) > 0 {
		nomosAgentLog.Infof("Agent turn waits for its attachments (run=%s pieces=%d reading=%d)", turn.RunID, len(pieces), len(reading))
		return runner.Expansion{
			Output: map[string]any{"attachments": len(pieces), "reading": len(reading)},
			Quiet:  true,
			Steps:  []runner.StepSpec{agentWaitSpec(1, sc.Step.StepKey, time.Now().Unix(), reading)},
		}, nil
	}
	return agentStartDecisions(sc, turn, map[string]any{"attachments": len(pieces), "reading": 0}, len(pieces), 0), nil
}

// agentWaitStep re-reads the ingestion state and either starts the work or
// schedules the next look with available_at: no worker ever sleeps on it,
// and no browser goroutine is involved. Past agentWaitTimeout the work starts
// with what is readable; the tools report what is still pending.
func agentWaitStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentWaitInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	reading := attachmentsBeingRead(repo, in.Pieces)
	waited := time.Since(time.Unix(in.Since, 0))
	if len(reading) == 0 || waited >= agentWaitTimeout {
		if len(reading) > 0 {
			nomosAgentLog.Warnf("Attachments still unread after %s, starting with what is readable (run=%s unread=%d)",
				agentWaitTimeout, turn.RunID, len(reading))
		}
		return agentStartDecisions(sc, turn, map[string]any{"waited_s": int(waited.Seconds()), "unread": len(reading)},
			len(in.Pieces), len(reading)), nil
	}
	return runner.Expansion{
		Output: map[string]any{"reading": len(reading)},
		Quiet:  true,
		Steps:  []runner.StepSpec{agentWaitSpec(in.N+1, sc.Step.StepKey, in.Since, reading)},
	}, nil
}

func agentWaitSpec(n int, after string, since int64, pieces []string) runner.StepSpec {
	delay := agentWaitDelay << (n - 1)
	if delay > agentWaitMaxDelay || delay <= 0 {
		delay = agentWaitMaxDelay
	}
	return runner.StepSpec{
		Key: agentWaitKey(n), Kind: stepAgentWait, Label: "Lecture des documents en cours",
		Input: agentWaitInput{N: n, Since: since, Pieces: pieces}, DependsOn: []string{after}, Delay: delay,
	}
}

// agentStartDecisions expands decision 1. The active-work deadline starts
// now, not at intake, and a turn that was `reading` its attachments becomes
// `streaming`.
func agentStartDecisions(sc *runner.StepCtx, turn *types.AgentTurn, output any, pieces, unread int) runner.Expansion {
	return runner.Expansion{
		Output: output,
		Quiet:  true,
		Steps:  []runner.StepSpec{agentNextDecision(1, sc.Step.StepKey)},
		Commit: agentCommit(sc.TenantID, func(tx *u.TenantRepo) error {
			now := time.Now().Unix()
			if err := tx.AgentTurns().Where("run_id = ?", turn.RunID).Updates(map[string]any{
				"deadline_at": now + int64(types.DefaultAgentActiveSeconds), "updated_at": now,
			}).Error; err != nil {
				return err
			}
			if pieces == 0 || turn.AssistantMessageID == nil {
				return nil
			}
			res := tx.Messages().Where("id = ? AND state = ?", *turn.AssistantMessageID, types.MessageStateReading).
				Updates(map[string]any{"state": types.MessageStateStreaming, "updated_at": now})
			if res.Error != nil || res.RowsAffected == 0 {
				return res.Error
			}
			_, err := agentEvent(tx, turn, EventAttachmentsRead, map[string]any{
				"message_id": *turn.AssistantMessageID, "pieces": pieces, "unread": unread,
			})
			return err
		}),
	}
}

// agentTurnAttachments are the piece ids attached to the turn's message.
func agentTurnAttachments(repo *u.TenantRepo, turn *types.AgentTurn) []string {
	if turn.UserMessageID == nil {
		return nil
	}
	var msg types.Message
	if err := repo.Messages().Where("id = ?", *turn.UserMessageID).Take(&msg).Error; err != nil {
		return nil
	}
	var ids []string
	if len(msg.Attachments) > 0 {
		_ = json.Unmarshal(msg.Attachments, &ids)
	}
	return ids
}

// ---------------------------------------------------------------------------
// The decision (§3.4, §3.5)
// ---------------------------------------------------------------------------

// agentDecideStep makes ONE controller dispatch. The dispatch is reserved in
// Postgres first — under the lease, below the ceiling, with its llm_calls
// row and input manifest — and the response is recorded before anything
// acts on it. A later attempt of this step resumes from a recorded response
// without paying again; a dispatch whose outcome is unknown keeps its slot,
// and the retry reserves the next one.
func agentDecideStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentDecideInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	if AgentTools == nil || !u.LLMConfigured() {
		return agentEnd(sc, nil, types.AgentOutcomeFailed, agentCodeRuntimeUnavailable, nil), nil
	}
	defs := agentToolDefinitions()

	var rec agentDecisionRecord
	if decodeRecord(sc.Step.Output, &rec) && rec.Recorded {
		nomosAgentLog.Infof("Resuming a recorded dispatch without a new call (run=%s decision=%d dispatch=%d attempt=%d)",
			turn.RunID, in.Decision, rec.Dispatch, sc.Step.AttemptCount)
		return acceptAgentDecision(ctx, sc, repo, turn, rec, defs)
	}

	rec = agentDecisionRecord{Decision: in.Decision}
	if time.Now().Unix() >= turn.DeadlineAt {
		rec.Ended = types.AgentOutcomeDeadline
		return agentEnd(sc, rec, types.AgentOutcomeDeadline, "", nil), nil
	}
	actx, err := BuildAgentContext(ctx, repo, turn, in.Decision, defs)
	if err != nil {
		return nil, err
	}
	opts := u.ToolChatOpts{
		Model:         turn.ControllerModel,
		Messages:      actx.Messages,
		Tools:         actx.Tools,
		MaxTokens:     agentMaxCompletionTokens,
		Purpose:       agentPurpose,
		TenantID:      sc.TenantID,
		RunID:         sc.Run.ID,
		RunStepID:     sc.Step.ID,
		StepAttemptID: sc.Attempt.ID,
		ActorID:       turn.ActorID,
	}
	sha, err := u.ToolRequestSHA256(opts.Model, opts.Messages, opts.Tools)
	if err != nil {
		return nil, err
	}
	opts.InputManifestSHA256 = sha
	actx.Manifest.SHA256 = sha

	dispatch, err := reserveAgentDispatch(ctx, sc, &opts)
	if errors.Is(err, errAgentCapReached) {
		rec.Ended = types.AgentOutcomeTurnLimit
		nomosAgentLog.Warnf("Dispatch ceiling reached, no further model call (run=%s decision=%d max=%d)",
			turn.RunID, in.Decision, turn.MaxDispatches)
		return agentEnd(sc, rec, types.AgentOutcomeTurnLimit, "", nil), nil
	}
	if err != nil {
		return nil, err
	}

	res, callErr := u.ChatTools(ctx, opts)
	rec.Dispatch = dispatch
	rec.LLMCallID = res.LLMCallID
	rec.Model = res.Model
	rec.FinishReason = res.FinishReason
	rec.Content = res.Content
	rec.RawCalls = res.ToolCalls
	rec.ReasoningDetails = res.ReasoningDetails
	rec.Manifest = actx.Manifest
	rec.Consumed = actx.Consumed
	switch {
	case callErr == nil:
	case errors.Is(callErr, u.ErrRefused):
		rec.Unusable = types.AgentOutcomeModelRefusal
	case errors.Is(callErr, u.ErrTruncated):
		rec.Unusable = types.AgentOutcomeModelTruncated
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(callErr, u.ErrAccounting):
		return nil, runner.Retry("llm_accounting_failed", "the controller dispatch could not be durably accounted")
	default:
		if sc.Step.AttemptCount < sc.Step.MaxAttempts {
			return nil, runner.Retry("controller_dispatch_failed", "the controller dispatch failed; the retry reserves a new dispatch")
		}
		rec.Ended = types.AgentOutcomeFailed
		return agentEnd(sc, rec, types.AgentOutcomeFailed, agentCodeGatewayUnavailable, nil), nil
	}

	rec.Recorded = true
	if err := agentRecordDecision(ctx, sc, rec); err != nil {
		return nil, err
	}
	if hook := agentDecisionRecorded; hook != nil {
		if err := hook(sc.Run.ID, rec.Dispatch); err != nil {
			return nil, err
		}
	}
	return acceptAgentDecision(ctx, sc, repo, turn, rec, defs)
}

// reserveAgentDispatch takes the next dispatch number — only while below the
// turn's ceiling, itself never above types.MaxAgentDispatches — and writes
// its llm_calls row in the same lease-fenced transaction.
func reserveAgentDispatch(ctx context.Context, sc *runner.StepCtx, opts *u.ToolChatOpts) (int, error) {
	var dispatch int
	err := sc.WithLeaseTransaction(ctx, func(tx *gorm.DB) error {
		repo := u.ForTenantTx(tx, sc.TenantID)
		var counts []int
		if err := repo.AgentTurns().Raw(`
			UPDATE agent_turns
			   SET dispatches = dispatches + 1, updated_at = EXTRACT(EPOCH FROM now())::bigint
			 WHERE tenant_id = ? AND run_id = ? AND dispatches < LEAST(max_dispatches, ?)
			RETURNING dispatches`, sc.TenantID, sc.Run.ID, types.MaxAgentDispatches).Scan(&counts).Error; err != nil {
			return err
		}
		if len(counts) == 0 {
			return errAgentCapReached
		}
		dispatch = counts[0]
		opts.AgentDispatch = dispatch
		row := u.NewLLMCallRecord(*opts)
		if err := repo.Create(&row); err != nil {
			return err
		}
		opts.Reserved = &u.ReservedCall{ID: row.ID, ExecutionID: row.ExecutionID}
		return nil
	})
	return dispatch, err
}

// agentRecordDecision makes the response durable on the step row before it is
// acted on, under the lease.
func agentRecordDecision(ctx context.Context, sc *runner.StepCtx, rec agentDecisionRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return sc.WithLeaseTransaction(ctx, func(tx *gorm.DB) error {
		return u.ForTenantTx(tx, sc.TenantID).RunSteps().Where("id = ?", sc.Step.ID).
			Update("output", types.JSONB(raw)).Error
	})
}

// acceptAgentDecision validates the recorded response and commits the steps
// it leads to, with the consumed reading receipts and the `running` activity
// rows, in one transaction.
func acceptAgentDecision(ctx context.Context, sc *runner.StepCtx, repo *u.TenantRepo, turn *types.AgentTurn,
	rec agentDecisionRecord, defs []types.AgentToolDefinition) (any, error) {
	n := rec.Decision
	byName := make(map[string]types.AgentToolDefinition, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}
	agentValidateCalls(&rec, byName)
	agentRefuseOverBudget(&rec, turn)

	consumed := rec.Consumed
	if rec.Unusable != "" {
		consumed = nil // a refused or truncated response accepted no input
	}
	markConsumed := func(tx *u.TenantRepo) error {
		if len(consumed) == 0 {
			return nil
		}
		return tx.AgentReadReceipts().Where("run_id = ? AND id IN ? AND consumed_dispatch IS NULL", turn.RunID, consumed).
			Update("consumed_dispatch", rec.Dispatch).Error
	}

	var executable []int
	for i := range rec.Calls {
		if _, failed := rec.Prefailed[i]; !failed {
			executable = append(executable, i)
		}
	}
	if len(executable) == 0 {
		nomosAgentLog.Infof("Decision rejected (run=%s decision=%d dispatch=%d rejection=%s calls=%d)",
			turn.RunID, n, rec.Dispatch, rec.Rejection, len(rec.Calls))
		if agentNoProgress(repo, turn.RunID, rec.Signatures) {
			rec.Ended = types.AgentOutcomeNoProgress
			return agentEnd(sc, rec, types.AgentOutcomeNoProgress, "", markConsumed), nil
		}
		return runner.Expansion{
			Output: rec,
			Quiet:  true,
			Steps:  []runner.StepSpec{agentNextDecision(n+1, sc.Step.StepKey)},
			Commit: agentCommit(sc.TenantID, markConsumed),
		}, nil
	}

	// Reads run in parallel; a batch holding a write runs serially, in the
	// order the controller gave.
	serial := false
	for _, i := range executable {
		if !byName[rec.Calls[i].Name].ReadOnly {
			serial = true
		}
	}
	var (
		specs    []runner.StepSpec
		toolKeys []string
		running  []types.AgentActivityPayload
	)
	scope := agentScopeOnce(ctx, repo, turn)
	for _, i := range executable {
		call := rec.Calls[i]
		def := byName[call.Name]
		deps := []string{sc.Step.StepKey}
		if serial && len(toolKeys) > 0 {
			deps = append(deps, toolKeys[len(toolKeys)-1])
		}
		note := agentProgressNote(call.Arguments)
		key := agentToolKey(n, i)
		specs = append(specs, runner.StepSpec{
			Key: key, Kind: agentToolStepKind(def.Class), Label: "Outil " + call.Name,
			Input:     agentToolInput{Decision: n, Dispatch: rec.Dispatch, Ordinal: i, Call: call, ProgressNote: note},
			DependsOn: deps,
		})
		toolKeys = append(toolKeys, key)
		object, counts := agentRunningObject(ctx, repo, turn, call, scope)
		running = append(running, types.AgentActivityPayload{
			Dispatch: rec.Dispatch, CallID: call.ID, Tool: call.Name, ProgressNote: note,
			Status: types.ActivityRunning, Object: object, Counts: counts,
		})
	}
	only := ""
	if len(rec.Calls) == 1 {
		only = rec.Calls[0].Name
	}
	switch only {
	case types.ToolAskLawyer:
		// The tool step parks the questions itself once they are validated.
	case types.ToolSubmitResult:
		call := rec.Calls[0]
		specs = append(specs, runner.StepSpec{
			Key: agentGateKey(n), Kind: stepAgentGate, Label: "Vérification finale",
			Input:     agentGateInput{Decision: n, Dispatch: rec.Dispatch, Call: call, ProgressNote: agentProgressNote(call.Arguments)},
			DependsOn: toolKeys,
		})
	default:
		specs = append(specs, runner.StepSpec{
			Key: agentObserveKey(n), Kind: stepAgentObserve, Label: "Observation",
			Input: agentObserveInput{Decision: n, Dispatch: rec.Dispatch}, DependsOn: toolKeys,
		})
	}
	nomosAgentLog.Infof("Decision accepted (run=%s decision=%d dispatch=%d model=%s calls=%d executed=%d refused=%d serial=%t consumed=%d)",
		turn.RunID, n, rec.Dispatch, rec.Model, len(rec.Calls), len(executable), len(rec.Prefailed), serial, len(consumed))
	return runner.Expansion{
		Output: rec,
		Steps:  specs,
		Quiet:  true,
		Commit: agentCommit(sc.TenantID, markConsumed, func(tx *u.TenantRepo) error {
			for _, p := range running {
				if err := emitAgentActivity(tx, turn, p); err != nil {
					return err
				}
			}
			return nil
		}),
	}, nil
}

// agentValidateCalls reads the recorded response (§5.1 ValidateAgentCalls):
// a supported tool, arguments that satisfy its strict schema, a valid public
// progress note, at most MaxAgentParallelReads calls, and ask_lawyer /
// submit_result alone. A refused call gets an observation the next decision
// sees; the valid calls of a batch still run, except when the count or the
// exclusivity rule refuses the whole batch.
func agentValidateCalls(rec *agentDecisionRecord, byName map[string]types.AgentToolDefinition) {
	rec.Calls, rec.Prefailed, rec.Rejection, rec.Signatures = nil, nil, "", nil
	if rec.Unusable != "" {
		rec.Rejection = rec.Unusable
		rec.Signatures = []string{"rejection:" + rec.Unusable}
		return
	}
	if len(rec.RawCalls) == 0 {
		rec.Rejection = types.AgentOutcomeSubmissionRequired
		rec.Signatures = []string{"rejection:" + types.AgentOutcomeSubmissionRequired}
		return
	}

	validJSON := make([]bool, len(rec.RawCalls))
	seen := map[string]bool{}
	for i, raw := range rec.RawCalls {
		pid := strings.TrimSpace(raw.ID)
		if pid == "" || seen[pid] {
			pid = fmt.Sprintf("call_%d_%d", rec.Dispatch, i)
		}
		seen[pid] = true
		args := json.RawMessage(raw.Arguments)
		validJSON[i] = json.Valid(args)
		if !validJSON[i] {
			args, _ = json.Marshal(raw.Arguments) // kept, as text, for the transcript
		}
		rec.Calls = append(rec.Calls, types.AgentToolCall{
			ID: types.AgentCallID(rec.Dispatch, i), ProviderID: pid, Name: raw.Name, Arguments: args,
		})
	}

	fail := func(i int, code, detail string) {
		if rec.Prefailed == nil {
			rec.Prefailed = map[int]types.ToolObservation{}
		}
		call := rec.Calls[i]
		rec.Prefailed[i] = types.ToolObservation{
			CallID: call.ID, Tool: call.Name, Status: types.ObservationError, Code: code, Retryable: true,
			Data: agentCallProblem(code, detail), EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
		}
		rec.Signatures = append(rec.Signatures, agentFailureSignature(call, code))
	}
	all := func(code string) {
		for i := range rec.Calls {
			fail(i, code, "")
		}
		rec.Rejection = types.AgentOutcomeInvalidToolCall
	}

	if len(rec.Calls) > types.MaxAgentParallelReads {
		all(types.ToolCodeTooManyCalls)
		return
	}
	if len(rec.Calls) > 1 {
		for _, c := range rec.Calls {
			if byName[c.Name].Exclusive {
				all(types.ToolCodeExclusiveCall)
				return
			}
		}
	}
	for i, c := range rec.Calls {
		def, ok := byName[c.Name]
		switch {
		case !ok:
			fail(i, types.ToolCodeUnknownTool, clipRunes(c.Name, 64))
		case !validJSON[i]:
			fail(i, types.ToolCodeInvalidArguments, "les arguments ne sont pas un objet JSON valide")
		default:
			if err := u.ValidateToolArguments(c.Arguments, def.Parameters); err != nil {
				fail(i, types.ToolCodeInvalidArguments, err.Error())
			} else if note := agentProgressNote(c.Arguments); note == "" ||
				utf8.RuneCountInString(note) > types.MaxProgressNoteRunes {
				fail(i, types.ToolCodeInvalidArguments,
					fmt.Sprintf("progress_note doit être une phrase publique de 1 à %d caractères", types.MaxProgressNoteRunes))
			}
		}
	}
	if len(rec.Prefailed) == len(rec.Calls) {
		rec.Rejection = types.AgentOutcomeInvalidToolCall
	}
}

// agentRefuseOverBudget refuses, at acceptance, the review and web calls a
// batch holds beyond what remains of their separate budgets (§2), so calls
// running in parallel can never spend past the ceiling together. Every
// earlier call has finished by now — a decision waits for its predecessor's
// observations — so the turn's counters are final.
func agentRefuseOverBudget(rec *agentDecisionRecord, turn *types.AgentTurn) {
	left := agentBudget(turn)
	remaining := map[string]int{
		types.ToolReviewOutput: left.ReviewCallsLeft,
		types.ToolSearchWeb:    left.WebSearchesLeft,
	}
	for i, call := range rec.Calls {
		budget, metered := remaining[call.Name]
		if !metered {
			continue
		}
		if _, failed := rec.Prefailed[i]; failed {
			continue
		}
		if budget > 0 {
			remaining[call.Name] = budget - 1
			continue
		}
		if rec.Prefailed == nil {
			rec.Prefailed = map[int]types.ToolObservation{}
		}
		rec.Prefailed[i] = types.ToolObservation{
			CallID: call.ID, Tool: call.Name, Status: types.ObservationError, Code: types.ToolCodeBudgetExhausted,
			Data: agentCallProblem(types.ToolCodeBudgetExhausted,
				"le budget de cet outil est épuisé pour cette demande : poursuis sans lui et signale la limite"),
			EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
		}
		rec.Signatures = append(rec.Signatures, agentFailureSignature(call, types.ToolCodeBudgetExhausted))
	}
	if len(rec.Calls) > 0 && len(rec.Prefailed) == len(rec.Calls) && rec.Rejection == "" {
		rec.Rejection = types.AgentOutcomeInvalidToolCall
	}
}

func agentProgressNote(args json.RawMessage) string {
	var a types.ToolArgs
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return strings.TrimSpace(a.ProgressNote)
}

// agentFailureSignature identifies a failing call by tool, arguments (the
// progress note aside, which changes with every retelling) and error code.
func agentFailureSignature(call types.AgentToolCall, code string) string {
	canon := string(call.Arguments)
	var args map[string]any
	if json.Unmarshal(call.Arguments, &args) == nil {
		delete(args, types.ProgressNoteArg)
		if b, err := json.Marshal(args); err == nil {
			canon = string(b)
		}
	}
	sum := sha256.Sum256([]byte(call.Name + "\x00" + canon + "\x00" + code))
	return hex.EncodeToString(sum[:])[:16]
}

// agentNoProgress reports whether one failure signature has now occurred
// MaxAgentRepeatedFailures times in the run (§3.7), counting extra — the
// signatures of the step being committed — with the recorded ones.
func agentNoProgress(repo *u.TenantRepo, runID string, extra []string) bool {
	counts := map[string]int{}
	for _, s := range extra {
		counts[s]++
	}
	var steps []types.RunStep
	if err := repo.RunSteps().Select("kind", "output").
		Where("run_id = ? AND kind IN ? AND output IS NOT NULL", runID,
			[]string{stepAgentDecide, stepAgentToolDB, stepAgentToolExt, stepAgentToolRender, stepAgentGate, stepAgentPublish}).
		Find(&steps).Error; err != nil {
		nomosAgentLog.Warnf("No-progress check could not read the run (run=%s): %v", runID, err)
	}
	for _, s := range steps {
		switch s.Kind {
		case stepAgentDecide:
			var rec agentDecisionRecord
			if decodeRecord(s.Output, &rec) {
				for _, sig := range rec.Signatures {
					counts[sig]++
				}
			}
		case stepAgentGate, stepAgentPublish:
			var rec agentGateRecord
			if decodeRecord(s.Output, &rec) && rec.Signature != "" {
				counts[rec.Signature]++
			}
		default:
			var rec agentToolRecord
			if decodeRecord(s.Output, &rec) && rec.Signature != "" {
				counts[rec.Signature]++
			}
		}
	}
	for _, n := range counts {
		if n >= types.MaxAgentRepeatedFailures {
			return true
		}
	}
	return false
}

func agentToolStepKind(class string) string {
	switch class {
	case types.ToolClassExternal:
		return stepAgentToolExt
	case types.ToolClassRender:
		return stepAgentToolRender
	default:
		return stepAgentToolDB
	}
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// agentToolStep executes one accepted call through the registry. Expected
// failures are observations. An error from the tool is retried while the
// step has attempts left (a dropped Postgres connection, a timeout); it fails
// the turn on the last attempt, or at once when it is permanent. The tool's
// rows (Persist), the budget counters and the final activity row commit with
// the step's success.
func agentToolStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentToolInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}

	var outcome ToolOutcome
	started := time.Now()
	tool, ok := lookupAgentTool(in.Call.Name)
	if !ok {
		outcome.Observation = types.ToolObservation{
			Status: types.ObservationError, Code: types.ToolCodeToolUnavailable, Retryable: false,
			Data: agentCallProblem(types.ToolCodeToolUnavailable, "cet outil n'est plus disponible"),
		}
	} else {
		x := ToolExecution{
			TenantID:       sc.TenantID,
			ActorID:        turn.ActorID,
			RunID:          sc.Run.ID,
			StepID:         sc.Step.ID,
			StepAttemptID:  sc.Attempt.ID,
			ExecutionID:    sc.ExecutionID,
			Dispatch:       in.Dispatch,
			CallID:         in.Call.ID,
			ConversationID: turn.ConversationID,
			Contract:       turnContract(turn),
			Permissions:    agentPermissions(repo, turn),
			Budget:         agentBudget(turn),
			Heartbeat:      sc.Heartbeat,
		}
		out, err := tool.Execute(ctx, x, in.Call.Arguments)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var se *runner.StepError
			permanent := (errors.As(err, &se) && !se.Retryable) || errors.Is(err, errDossierAccessRevoked)
			if !permanent && sc.Step.AttemptCount < sc.Step.MaxAttempts {
				nomosAgentLog.Warnf("Tool failed, retrying (run=%s tool=%s call=%s attempt=%d of %d): %v",
					turn.RunID, in.Call.Name, in.Call.ID, sc.Step.AttemptCount, sc.Step.MaxAttempts, err)
				return nil, runner.Retry("agent_tool_failed", "the tool failed; the call is retried")
			}
			nomosAgentLog.Errorf("Tool failed the turn (run=%s tool=%s call=%s attempt=%d permanent=%t): %v",
				turn.RunID, in.Call.Name, in.Call.ID, sc.Step.AttemptCount, permanent, err)
			failed := types.AgentActivityPayload{
				Dispatch: in.Dispatch, CallID: in.Call.ID, Tool: in.Call.Name, ProgressNote: in.ProgressNote,
				Status: types.ActivityError, Object: types.AgentActivityObject{Kind: activityKindFor(in.Call.Name)},
			}
			return agentEnd(sc, agentToolRecord{Call: in.Call}, types.AgentOutcomeFailed, agentCodeToolFailed,
				func(tx *u.TenantRepo) error { return emitAgentActivity(tx, turn, failed) }), nil
		}
		outcome = out
	}

	obs := agentNormalizeObservation(outcome.Observation, in.Call)
	record := agentToolRecord{Call: in.Call, Observation: obs}
	if obs.Status == types.ObservationError {
		record.Signature = agentFailureSignature(in.Call, obs.Code)
	}
	activity := types.AgentActivityPayload{
		Dispatch: in.Dispatch, CallID: in.Call.ID, Tool: in.Call.Name, ProgressNote: in.ProgressNote,
		Status: obs.Status, Object: outcome.Activity, Counts: outcome.Counts,
	}
	if obs.Status == types.ObservationError {
		// The code alone says why the call ended — e.g. a public query the
		// confidentiality gate kept inside the cabinet — never its content.
		activity.Code = obs.Code
	}
	nomosAgentLog.Infof("Tool executed (run=%s tool=%s call=%s status=%s code=%s evidence=%d receipts=%d ms=%d)",
		turn.RunID, in.Call.Name, in.Call.ID, obs.Status, obs.Code, len(obs.EvidenceIDs), len(obs.ReadReceiptIDs),
		time.Since(started).Milliseconds())

	// review_output and search_web reserve their budget slot on agent_turns
	// themselves, durably and before the paid call; counting them here too
	// would spend each slot twice.
	persist := func(tx *u.TenantRepo) error {
		if outcome.Persist != nil {
			return outcome.Persist(tx)
		}
		return nil
	}
	if in.Call.Name == types.ToolAskLawyer {
		return agentAskExpansion(sc, turn, in, record, activity, persist), nil
	}
	return runner.Expansion{
		Output: record,
		Quiet:  true,
		Commit: agentCommit(sc.TenantID, persist, func(tx *u.TenantRepo) error {
			return emitAgentActivity(tx, turn, activity)
		}),
	}, nil
}

func lookupAgentTool(name string) (AgentTool, bool) {
	if AgentTools == nil {
		return nil, false
	}
	return AgentTools.Lookup(name)
}

// agentNormalizeObservation stamps the server's identity on the envelope and
// turns a malformed one into an explicit error.
func agentNormalizeObservation(obs types.ToolObservation, call types.AgentToolCall) types.ToolObservation {
	obs.CallID, obs.Tool = call.ID, call.Name
	switch obs.Status {
	case types.ObservationOK, types.ObservationEmpty, types.ObservationError:
	default:
		obs = types.ToolObservation{
			CallID: call.ID, Tool: call.Name, Status: types.ObservationError, Code: types.ToolCodeToolUnavailable,
			Data: agentCallProblem(types.ToolCodeToolUnavailable, "l'outil a rendu une observation invalide"),
		}
	}
	if obs.EvidenceIDs == nil {
		obs.EvidenceIDs = []string{}
	}
	if obs.ReadReceiptIDs == nil {
		obs.ReadReceiptIDs = []string{}
	}
	return obs
}

// agentPermissions resolves the actor's rights on the turn's dossier for
// this call — the creator, a per-dossier grant, or the firm role — so a
// revocation takes effect at the next call.
func agentPermissions(repo *u.TenantRepo, turn *types.AgentTurn) ToolPermissions {
	if turn.DossierID == nil {
		return ToolPermissions{}
	}
	var d types.Dossier
	if err := repo.Dossiers().Where("id = ?", *turn.DossierID).Take(&d).Error; err != nil {
		return ToolPermissions{}
	}
	if d.CreatedBy == turn.ActorID {
		return ToolPermissions{DossierRead: true, DossierWrite: true}
	}
	allowed := func(act string) bool {
		if ok, err := u.Enforce(turn.ActorID, repo.TenantID(), dossierObject(d.ID), act); err == nil && ok {
			return true
		}
		ok, err := u.Enforce(turn.ActorID, repo.TenantID(), types.ObjectDossier, act)
		return err == nil && ok
	}
	return ToolPermissions{DossierRead: allowed(types.ActionRead), DossierWrite: allowed(types.ActionWrite)}
}

func agentBudget(turn *types.AgentTurn) ToolBudget {
	b := ToolBudget{
		ReviewCallsLeft: types.MaxAgentReviewCalls - turn.ReviewCalls,
		WebSearchesLeft: types.MaxAgentWebSearches - turn.WebSearches,
	}
	if b.ReviewCallsLeft < 0 {
		b.ReviewCallsLeft = 0
	}
	if b.WebSearchesLeft < 0 {
		b.WebSearchesLeft = 0
	}
	return b
}

// ---------------------------------------------------------------------------
// ask_lawyer (§5.4): the existing ask_user parking, then observe/continue
// ---------------------------------------------------------------------------

// agentAskQuestions is the observation data an ask_lawyer tool returns with
// status ok: the questions it validated. The runtime re-checks the bounds,
// assigns run-unique keys and parks them through the built-in ask_user step.
type agentAskQuestions struct {
	Questions []struct {
		Prompt   string `json:"prompt"`
		Kind     string `json:"kind"`
		Required bool   `json:"required"`
		Options  []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

func agentAskExpansion(sc *runner.StepCtx, turn *types.AgentTurn, in agentToolInput, record agentToolRecord,
	activity types.AgentActivityPayload, persist func(tx *u.TenantRepo) error) runner.Expansion {
	n := in.Decision
	observe := runner.StepSpec{
		Key: agentObserveKey(n), Kind: stepAgentObserve, Label: "Observation",
		Input: agentObserveInput{Decision: n, Dispatch: in.Dispatch}, DependsOn: []string{sc.Step.StepKey},
	}
	questions, problem := agentParseQuestions(record.Observation, n)
	if record.Observation.Status != types.ObservationOK || problem != "" {
		if problem != "" {
			record.Observation = types.ToolObservation{
				CallID: in.Call.ID, Tool: in.Call.Name, Status: types.ObservationError,
				Code: types.ToolCodeInvalidArguments, Retryable: true,
				Data:        agentCallProblem(types.ToolCodeInvalidArguments, problem),
				EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
			}
			record.Signature = agentFailureSignature(in.Call, types.ToolCodeInvalidArguments)
			activity.Status = types.ActivityError
		}
		return runner.Expansion{
			Output: record, Quiet: true, Steps: []runner.StepSpec{observe},
			Commit: agentCommit(sc.TenantID, persist, func(tx *u.TenantRepo) error {
				return emitAgentActivity(tx, turn, activity)
			}),
		}
	}

	observe.Input = agentObserveInput{Decision: n, Dispatch: in.Dispatch, Ask: true}
	observe.DependsOn = []string{agentAskKey(n)}
	ask := runner.StepSpec{
		Key: agentAskKey(n), Kind: runner.StepKindAskUser, Label: "Question à l'avocat",
		Input: runner.AskUserInput{Questions: questions}, DependsOn: []string{sc.Step.StepKey},
	}
	activity.Status = types.ActivityRunning
	activity.Object = types.AgentActivityObject{Kind: types.ActivityObjectQuestion, Label: clipRunes(questions[0].Prompt, 140)}
	activity.Counts = &types.AgentActivityCounts{Questions: intPtr(len(questions))}
	nomosAgentLog.Infof("Questions parked for the lawyer (run=%s decision=%d questions=%d)", turn.RunID, n, len(questions))
	return runner.Expansion{
		Output: record, Quiet: true, Steps: []runner.StepSpec{ask, observe},
		Commit: agentCommit(sc.TenantID, persist, func(tx *u.TenantRepo) error {
			if err := emitAgentActivity(tx, turn, activity); err != nil {
				return err
			}
			// Waiting on the lawyer is not active work: the deadline is
			// pushed back by the parked time when the answers arrive.
			return tx.AgentTurns().Where("run_id = ?", turn.RunID).
				Update("parked_at", time.Now().Unix()).Error
		}),
	}
}

// agentParseQuestions bounds and keys the question set an ask_lawyer
// observation carries.
func agentParseQuestions(obs types.ToolObservation, n int) ([]runner.Question, string) {
	if obs.Status != types.ObservationOK {
		return nil, ""
	}
	var set agentAskQuestions
	if len(obs.Data) == 0 || json.Unmarshal(obs.Data, &set) != nil {
		return nil, "les questions ne sont pas lisibles"
	}
	if len(set.Questions) == 0 || len(set.Questions) > types.MaxAgentQuestionsPerSet {
		return nil, fmt.Sprintf("une demande porte sur 1 à %d questions", types.MaxAgentQuestionsPerSet)
	}
	out := make([]runner.Question, 0, len(set.Questions))
	for i, q := range set.Questions {
		prompt := strings.TrimSpace(q.Prompt)
		if prompt == "" || utf8.RuneCountInString(prompt) > 2000 {
			return nil, fmt.Sprintf("la question %d doit avoir un énoncé de 1 à 2000 caractères", i+1)
		}
		kind := q.Kind
		if kind == "" {
			kind = types.QuestionKindText
			if len(q.Options) > 0 {
				kind = types.QuestionKindSingle
			}
		}
		question := runner.Question{Key: agentQuestionKey(n, i+1), Prompt: prompt, Kind: kind, Required: q.Required}
		switch kind {
		case types.QuestionKindText:
			if len(q.Options) > 0 {
				return nil, fmt.Sprintf("la question %d est ouverte : elle ne propose pas d'options", i+1)
			}
		case types.QuestionKindSingle, types.QuestionKindMulti:
			if len(q.Options) < 2 || len(q.Options) > 12 {
				return nil, fmt.Sprintf("la question %d doit proposer de 2 à 12 options", i+1)
			}
			seen := map[string]bool{}
			for _, o := range q.Options {
				value, label := strings.TrimSpace(o.Value), strings.TrimSpace(o.Label)
				if value == "" || label == "" || seen[value] || utf8.RuneCountInString(value) > 128 ||
					utf8.RuneCountInString(label) > 300 {
					return nil, fmt.Sprintf("la question %d a une option vide, trop longue ou en double", i+1)
				}
				seen[value] = true
				question.Options = append(question.Options, runner.QuestionOption{Value: value, Label: label})
			}
		default:
			return nil, fmt.Sprintf("la question %d a un type inconnu (single, multi ou text)", i+1)
		}
		out = append(out, question)
	}
	return out, ""
}

// ---------------------------------------------------------------------------
// Observation and continuation
// ---------------------------------------------------------------------------

// agentObserveStep settles one decision: every observation of it is durable,
// in call order. It stops a loop that keeps failing the same way, then
// schedules the next decision.
func agentObserveStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentObserveInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	steps, err := agentRunSteps(repo, turn.RunID)
	if err != nil {
		return nil, err
	}
	var rec agentDecisionRecord
	if !decodeRecord(steps[agentDecisionKey(in.Decision)].Output, &rec) {
		return nil, runner.Permanent("agent_decision_missing", "the observed decision has no record")
	}
	out := agentObserveRecord{Decision: in.Decision, Statuses: map[string]string{}}
	for i, call := range rec.Calls {
		out.Statuses[call.ID] = agentObservationFor(steps, rec, in.Decision, i, call).Status
	}

	var unpark func(tx *u.TenantRepo) error
	if in.Ask && len(rec.Calls) == 1 {
		obs, answered, total := agentAskObservation(repo, steps, rec.Calls[0], in.Decision)
		out.AskObservation = &obs
		out.Statuses[rec.Calls[0].ID] = obs.Status
		done := types.AgentActivityPayload{
			Dispatch: in.Dispatch, CallID: rec.Calls[0].ID, Tool: types.ToolAskLawyer,
			ProgressNote: agentProgressNote(rec.Calls[0].Arguments), Status: types.ActivityOK,
			Object: types.AgentActivityObject{Kind: types.ActivityObjectQuestion, Label: agentAskLabel(obs)},
			Counts: &types.AgentActivityCounts{Questions: intPtr(total), Results: intPtr(answered)},
		}
		unpark = func(tx *u.TenantRepo) error {
			if err := tx.AgentTurns().Where("run_id = ? AND parked_at IS NOT NULL", turn.RunID).Updates(map[string]any{
				"deadline_at": gorm.Expr("deadline_at + GREATEST(0, ? - parked_at)", time.Now().Unix()),
				"parked_at":   nil,
			}).Error; err != nil {
				return err
			}
			return emitAgentActivity(tx, turn, done)
		}
		nomosAgentLog.Infof("Lawyer answers observed (run=%s decision=%d answered=%d of %d)", turn.RunID, in.Decision, answered, total)
	}

	if agentNoProgress(repo, turn.RunID, nil) {
		out.Ended = types.AgentOutcomeNoProgress
		return agentEnd(sc, out, types.AgentOutcomeNoProgress, "", unpark), nil
	}
	return runner.Expansion{
		Output: out,
		Quiet:  true,
		Steps:  []runner.StepSpec{agentNextDecision(in.Decision+1, sc.Step.StepKey)},
		Commit: agentCommit(sc.TenantID, unpark),
	}, nil
}

// agentAskObservation is what ask_lawyer observes once the parked step is
// complete: each question with its answer, or its recorded skip.
func agentAskObservation(repo *u.TenantRepo, steps map[string]types.RunStep, call types.AgentToolCall,
	n int) (types.ToolObservation, int, int) {
	type answerView struct {
		Key      string   `json:"key"`
		Prompt   string   `json:"prompt"`
		Required bool     `json:"required"`
		State    string   `json:"state"`
		Values   []string `json:"values,omitempty"`
		Text     string   `json:"text,omitempty"`
	}
	askStep := steps[agentAskKey(n)]
	var questions []types.RunQuestion
	if err := repo.RunQuestions().Where("run_id = ? AND run_step_id = ?", askStep.RunID, askStep.ID).
		Order("question_key").Find(&questions).Error; err != nil {
		nomosAgentLog.Warnf("Question read failed (run=%s decision=%d): %v", askStep.RunID, n, err)
	}
	views := make([]answerView, 0, len(questions))
	answered, requiredSkipped := 0, 0
	for _, q := range questions {
		v := answerView{Key: q.QuestionKey, Prompt: q.Prompt, Required: q.Required, State: q.State}
		var a struct {
			Values  []string `json:"values"`
			Text    string   `json:"text"`
			Skipped bool     `json:"skipped"`
		}
		if len(q.Answer) > 0 && json.Unmarshal(q.Answer, &a) == nil && !a.Skipped {
			v.Values, v.Text = a.Values, a.Text
		}
		switch q.State {
		case types.QuestionStateAnswered:
			answered++
		case types.QuestionStateSkipped:
			if q.Required {
				requiredSkipped++
			}
		}
		views = append(views, v)
	}
	data := map[string]any{"questions": views, "required_skipped": requiredSkipped}
	if requiredSkipped > 0 {
		data["message"] = "L'avocat n'a pas répondu à une question indispensable : ne remplace pas cette information " +
			"par une hypothèse. Produis un travail explicitement partiel qui le dit, ou explique la limite."
	}
	raw, _ := json.Marshal(data)
	return types.ToolObservation{
		CallID: call.ID, Tool: call.Name, Status: types.ObservationOK, Data: raw,
		EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
	}, answered, len(questions)
}

func agentAskLabel(obs types.ToolObservation) string {
	var data struct {
		Questions []struct {
			Prompt string `json:"prompt"`
		} `json:"questions"`
	}
	if json.Unmarshal(obs.Data, &data) == nil && len(data.Questions) > 0 {
		return clipRunes(data.Questions[0].Prompt, 140)
	}
	return ""
}

// ---------------------------------------------------------------------------
// The final gate (§7.1) and publication (§7.6)
// ---------------------------------------------------------------------------

// agentSubmissionData is the observation data a submit_result tool returns
// with status ok. The answer revision (kind `answer`) is found by its id or
// by the submitting call; document revisions are named explicitly.
type agentSubmissionData struct {
	AnswerRevisionID    string   `json:"answer_revision_id"`
	DocumentRevisionIDs []string `json:"document_revision_ids"`
	Outcome             string   `json:"outcome"`
}

// agentGateStep runs the ONE verifier on the exact submitted revisions. A
// refusal goes back to the controller as submit_result's observation, with
// every issue; only a submission that passes is scheduled for publication.
func agentGateStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentGateInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	n := in.Decision
	out := agentGateRecord{Decision: n}
	next := agentNextDecision(n+1, sc.Step.StepKey)

	steps, err := agentRunSteps(repo, turn.RunID)
	if err != nil {
		return nil, err
	}
	var tool agentToolRecord
	if !decodeRecord(steps[agentToolKey(n, 0)].Output, &tool) {
		return nil, runner.Permanent("agent_submission_missing", "the submission has no tool record")
	}
	if tool.Observation.Status != types.ObservationOK {
		// The tool itself refused the submission; its observation stands.
		if agentNoProgress(repo, turn.RunID, nil) {
			out.Ended = types.AgentOutcomeNoProgress
			return agentEnd(sc, out, types.AgentOutcomeNoProgress, "", nil), nil
		}
		return runner.Expansion{Output: out, Quiet: true, Steps: []runner.StepSpec{next}}, nil
	}

	answer, docs, outcome, problem := agentParseSubmission(repo, turn, in.Call, tool.Observation)
	if problem != "" {
		obs := types.ToolObservation{
			CallID: in.Call.ID, Tool: in.Call.Name, Status: types.ObservationError,
			Code: types.ToolCodeInvalidArguments, Retryable: true,
			Data: agentCallProblem(types.ToolCodeInvalidArguments, problem), EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
		}
		out.Observation, out.Signature = &obs, agentFailureSignature(in.Call, obs.Code)
		if agentNoProgress(repo, turn.RunID, []string{out.Signature}) {
			out.Ended = types.AgentOutcomeNoProgress
			return agentEnd(sc, out, types.AgentOutcomeNoProgress, "", nil), nil
		}
		return runner.Expansion{Output: out, Quiet: true, Steps: []runner.StepSpec{next}}, nil
	}

	if VerifyOutput == nil {
		out.Ended = types.AgentOutcomeRefused
		return agentEnd(sc, out, types.AgentOutcomeRefused, agentCodeVerificationUnavailable, nil), nil
	}
	revisions := append([]types.AgentDocumentRevision{answer}, docs...)
	reports := make([]types.VerificationReport, 0, len(revisions))
	refused, incomplete := false, false
	warnings, blocking := 0, 0
	for _, rev := range revisions {
		report, err := VerifyOutput(ctx, VerificationInput{
			TenantID: sc.TenantID, RunID: sc.Run.ID, DocumentRevisionID: rev.ID, Final: true,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			nomosAgentLog.Warnf("Final verification could not run (run=%s revision=%s): %v", turn.RunID, rev.ID, err)
			return nil, runner.Retry("verification_failed", "the final verification could not run")
		}
		if report.DocumentRevisionID != rev.ID || report.ContentSHA256 != rev.ContentSHA256 {
			// A report that does not bind this exact revision is no report.
			report.DocumentRevisionID, report.ContentSHA256 = rev.ID, rev.ContentSHA256
			report.Result, report.DownloadAllowed = types.VerificationBlocked, false
			report.Blocking++
			report.Issues = append(report.Issues, types.VerificationIssue{
				Code: types.IssueRevisionHashMismatch, Severity: types.SeverityBlocking, Check: types.CheckOwnership,
			})
		}
		if report.Result == types.VerificationBlocked || report.Blocking > 0 || !report.DownloadAllowed {
			refused = true
		}
		if outcome == types.AgentOutcomeCompleted && coverageIncomplete(report) {
			// An exhaustive request read in part may be delivered, but only
			// as partial: refused here, so the agent can read on or resubmit,
			// rather than at publication where it could only fail the run.
			incomplete = true
		}
		warnings += report.Warnings
		blocking += report.Blocking
		reports = append(reports, report)
	}

	store := func(tx *u.TenantRepo) error { return agentStoreVerifications(tx, turn.RunID, reports) }
	activity := types.AgentActivityPayload{
		Dispatch: in.Dispatch, CallID: in.Call.ID, Tool: types.ToolSubmitResult, ProgressNote: in.ProgressNote,
		Status: types.ActivityOK,
		Object: types.AgentActivityObject{Kind: types.ActivityObjectVerification, Label: "Vérification finale"},
		Counts: &types.AgentActivityCounts{Warnings: intPtr(warnings), Blocking: intPtr(blocking)},
	}
	nomosAgentLog.Infof("Final gate (run=%s decision=%d revisions=%d refused=%t incomplete=%t warnings=%d blocking=%d)",
		turn.RunID, n, len(revisions), refused, incomplete, warnings, blocking)

	if refused || incomplete {
		activity.Status = types.ActivityError
		code := types.ToolCodeVerificationRefused
		message := "La vérification finale refuse la soumission : corrige les points bloquants (ou retire " +
			"l'affirmation concernée), vérifie de nouveau, puis soumets."
		switch {
		case !refused:
			message = "La demande porte sur l'ensemble du dossier et toutes les pièces n'ont pas été lues intégralement : " +
				"lis les pièces manquantes (read_piece), ou soumets ce résultat avec outcome « partial »."
		case !incomplete && onlyRetryableBlocking(reports):
			// The official source could not be asked (§7.3): nothing is
			// published, and the next decision waits before the agent
			// resubmits or finishes with an honest limitation.
			code = types.ToolCodeSourceUnavailable
			message = "La source officielle (Légifrance ou Judilibre) n'a pas pu être consultée : les citations juridiques " +
				"n'ont pas été revérifiées et rien n'est publié. Soumets de nouveau la même révision (chaque soumission " +
				"consomme un tour), ou soumets une réponse qui ne s'appuie pas sur ces citations et qui indique cette limite."
			next.Delay = agentSourceRetryDelay
		}
		raw, _ := json.Marshal(map[string]any{
			"message": message,
			"reports": reports,
		})
		obs := types.ToolObservation{
			CallID: in.Call.ID, Tool: in.Call.Name, Status: types.ObservationError,
			Code: code, Retryable: true, Data: raw,
			EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
		}
		out.Observation = &obs
		emit := func(tx *u.TenantRepo) error { return emitAgentActivity(tx, turn, activity) }
		end := func(outcome, failure string) (any, error) {
			out.Ended = outcome
			return agentEnd(sc, out, outcome, failure, func(tx *u.TenantRepo) error {
				if err := store(tx); err != nil {
					return err
				}
				return emit(tx)
			}), nil
		}
		if code == types.ToolCodeSourceUnavailable {
			// Resubmitting the same revision is what the agent was told to
			// do: an outage is never a no_progress signature. It has its own
			// bound and its own explanation, which names the source.
			if agentSourceOutages(repo, turn.RunID)+1 >= types.MaxAgentSourceOutages {
				return end(types.AgentOutcomeFailed, agentCodeOfficialSourceUnavailable)
			}
		} else {
			out.Signature = agentFailureSignature(in.Call, obs.Code)
			if agentNoProgress(repo, turn.RunID, []string{out.Signature}) {
				return end(types.AgentOutcomeNoProgress, "")
			}
		}
		return runner.Expansion{
			Output: out, Quiet: true, Steps: []runner.StepSpec{next},
			Commit: agentCommit(sc.TenantID, store, emit),
		}, nil
	}

	out.Passed = true
	publish := agentPublishInput{
		Decision: n, Dispatch: in.Dispatch, CallID: in.Call.ID, Call: in.Call, ProgressNote: in.ProgressNote,
		Outcome: outcome, Answer: agentRefOf(reports[0]),
	}
	for _, r := range reports[1:] {
		publish.Documents = append(publish.Documents, agentRefOf(r))
	}
	return runner.Expansion{
		Output: out,
		Quiet:  true,
		Steps: []runner.StepSpec{{
			Key: agentPublishKey(n), Kind: stepAgentPublish, Label: "Publication du résultat vérifié",
			Input: publish, DependsOn: []string{sc.Step.StepKey},
		}},
		Commit: agentCommit(sc.TenantID, store, func(tx *u.TenantRepo) error {
			return emitAgentActivity(tx, turn, activity)
		}),
	}, nil
}

// agentSourceRetryDelay is how long the next decision waits after the final
// gate found an official source unreachable. A variable so tests can shorten
// it; MaxAgentSourceOutages, the dispatch ceiling and the deadline bound how
// often the wait repeats.
var agentSourceRetryDelay = 20 * time.Second

// agentSourceOutages counts the final-gate refusals of the run on an
// unreachable official source.
func agentSourceOutages(repo *u.TenantRepo, runID string) int {
	var steps []types.RunStep
	if err := repo.RunSteps().Select("output").
		Where("run_id = ? AND kind = ? AND output IS NOT NULL", runID, stepAgentGate).
		Find(&steps).Error; err != nil {
		nomosAgentLog.Warnf("Outage count could not read the run (run=%s): %v", runID, err)
	}
	n := 0
	for _, s := range steps {
		var rec agentGateRecord
		if decodeRecord(s.Output, &rec) && rec.Observation != nil && rec.Observation.Code == types.ToolCodeSourceUnavailable {
			n++
		}
	}
	return n
}

// onlyRetryableBlocking reports whether the reports block, and only on
// issues that may clear by themselves (an unreachable official source).
func onlyRetryableBlocking(reports []types.VerificationReport) bool {
	blocking := 0
	for _, r := range reports {
		for _, iss := range r.Issues {
			if iss.Severity != types.SeverityBlocking {
				continue
			}
			if !iss.Retryable {
				return false
			}
			blocking++
		}
	}
	return blocking > 0
}

func agentRefOf(r types.VerificationReport) agentVerifiedRef {
	return agentVerifiedRef{
		RevisionID: r.DocumentRevisionID, ContentSHA256: r.ContentSHA256, ScopeSHA256: r.ScopeSHA256,
		EvidenceSHA256: r.EvidenceSHA256, VerifierVersion: r.VerifierVersion,
	}
}

// agentParseSubmission resolves what submit_result submitted.
func agentParseSubmission(repo *u.TenantRepo, turn *types.AgentTurn, call types.AgentToolCall,
	obs types.ToolObservation) (types.AgentDocumentRevision, []types.AgentDocumentRevision, string, string) {
	var data agentSubmissionData
	if len(obs.Data) > 0 {
		_ = json.Unmarshal(obs.Data, &data)
	}
	var answer types.AgentDocumentRevision
	q := repo.AgentDocumentRevisions().Where("run_id = ? AND kind = ?", turn.RunID, types.AgentOutputAnswer)
	if id := strings.TrimSpace(data.AnswerRevisionID); id != "" {
		q = q.Where("id = ?", id)
	} else {
		q = q.Where("created_by_call_id = ?", call.ID)
	}
	if err := q.Take(&answer).Error; err != nil {
		return answer, nil, "", "submit_result n'a enregistré aucune réponse (révision de type answer) pour ce tour"
	}
	var docs []types.AgentDocumentRevision
	seen := map[string]bool{answer.ID: true}
	for _, id := range data.DocumentRevisionIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var rev types.AgentDocumentRevision
		if err := repo.AgentDocumentRevisions().Where("run_id = ? AND id = ?", turn.RunID, id).Take(&rev).Error; err != nil {
			return answer, nil, "", "révision de document inconnue dans ce travail : " + clipRunes(id, 64)
		}
		if rev.Kind == types.AgentOutputAnswer {
			return answer, nil, "", "une révision de type answer ne se soumet pas comme document"
		}
		docs = append(docs, rev)
	}
	if len(docs) > 10 {
		return answer, nil, "", "au plus 10 documents par soumission"
	}
	outcome := types.AgentOutcomeCompleted
	if data.Outcome == types.AgentOutcomePartial {
		outcome = types.AgentOutcomePartial
		// Delivery opens a partial answer with « ses limites sont indiquées
		// ci-dessous »: the verified answer itself must state them.
		if !answerStatesLimits(answer) {
			return answer, nil, "", "un résultat partiel doit indiquer ses limites dans la réponse elle-même : ajoute à la " +
				"révision answer un bloc de rôle « limitation » qui dit ce qui manque, vérifie-la de nouveau, puis soumets"
		}
	}
	return answer, docs, outcome, ""
}

// answerStatesLimits reports whether an answer revision carries a non-empty
// limitation block.
func answerStatesLimits(rev types.AgentDocumentRevision) bool {
	var body types.DocumentRevisionBody
	if json.Unmarshal(rev.Body, &body) != nil {
		return false
	}
	for _, b := range body.Blocks {
		if b.Role == types.BlockLimitation && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

// agentStoreVerifications stores the gate's reports, bound to their exact
// hashes. A report already stored for the same target is replaced by the
// final one: the final gate's verdict is authoritative for those hashes.
func agentStoreVerifications(tx *u.TenantRepo, runID string, reports []types.VerificationReport) error {
	// The upsert needs the transaction's bare handle; tenant_id is stamped
	// explicitly from the repository, never from the report.
	raw := tx.AgentVerifications().Session(&gorm.Session{NewDB: true})
	for _, r := range reports {
		body, err := json.Marshal(r)
		if err != nil {
			return err
		}
		row := types.AgentVerification{
			ID: uuid.NewString(), TenantID: tx.TenantID(), RunID: runID, DocumentRevisionID: r.DocumentRevisionID,
			ContentSHA256: r.ContentSHA256, ScopeSHA256: r.ScopeSHA256, EvidenceSHA256: r.EvidenceSHA256,
			VerifierVersion: r.VerifierVersion, Result: r.Result, DownloadAllowed: r.DownloadAllowed,
			Warnings: r.Warnings, Blocking: r.Blocking, Report: types.JSONB(body),
		}
		if err := raw.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "document_revision_id"}, {Name: "content_sha256"},
				{Name: "scope_sha256"}, {Name: "evidence_sha256"}, {Name: "verifier_version"}},
			DoUpdates: clause.AssignmentColumns([]string{"result", "download_allowed", "warnings", "blocking", "report"}),
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// agentLoadVerified loads a revision with the exact report the gate stored
// for it, and refuses anything that no longer matches.
func agentLoadVerified(repo *u.TenantRepo, runID string, ref agentVerifiedRef) (VerifiedRevision, error) {
	var v types.AgentVerification
	if err := repo.AgentVerifications().Where(
		"run_id = ? AND document_revision_id = ? AND content_sha256 = ? AND scope_sha256 = ? AND evidence_sha256 = ? AND verifier_version = ?",
		runID, ref.RevisionID, ref.ContentSHA256, ref.ScopeSHA256, ref.EvidenceSHA256, ref.VerifierVersion).
		Take(&v).Error; err != nil {
		return VerifiedRevision{}, err
	}
	var rev types.AgentDocumentRevision
	if err := repo.AgentDocumentRevisions().Where("run_id = ? AND id = ?", runID, ref.RevisionID).Take(&rev).Error; err != nil {
		return VerifiedRevision{}, err
	}
	if rev.ContentSHA256 != v.ContentSHA256 || v.Result == types.VerificationBlocked || !v.DownloadAllowed {
		return VerifiedRevision{}, fmt.Errorf("revision %s no longer matches its verification", rev.ID)
	}
	return VerifiedRevision{Revision: rev, Verification: v}, nil
}

// agentPublishStep renders the verified documents into staged objects, then
// publishes everything through the publisher's lease-fenced commit. The
// turn's outcome and final verification commit with the step, which then
// schedules delivery of the verified answer.
func agentPublishStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	var in agentPublishInput
	if err := sc.Input(&in); err != nil {
		return nil, err
	}
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.Outcome != "" {
		return nil, nil
	}
	if AgentPublisher == nil || (len(in.Documents) > 0 && AgentArtifacts == nil) {
		return agentEnd(sc, nil, types.AgentOutcomeRefused, agentCodePublicationUnavailable, nil), nil
	}
	answer, err := agentLoadVerified(repo, turn.RunID, in.Answer)
	if err != nil {
		nomosAgentLog.Errorf("Verified answer unavailable at publication (run=%s): %v", turn.RunID, err)
		return nil, runner.Permanent("verified_revision_missing", "the verified answer no longer matches its report")
	}
	staged := make([]StagedArtifact, 0, len(in.Documents))
	warnings := answer.Verification.Warnings
	for _, ref := range in.Documents {
		doc, err := agentLoadVerified(repo, turn.RunID, ref)
		if err != nil {
			nomosAgentLog.Errorf("Verified document unavailable at publication (run=%s revision=%s): %v", turn.RunID, ref.RevisionID, err)
			return nil, runner.Permanent("verified_revision_missing", "a verified document no longer matches its report")
		}
		s, err := AgentArtifacts.PrepareArtifact(ctx, doc, ArtifactFormatDOCX)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			nomosAgentLog.Warnf("Rendering for publication failed (run=%s revision=%s): %v", turn.RunID, ref.RevisionID, err)
			return nil, runner.Retry("render_failed", "a verified document could not be rendered")
		}
		warnings += doc.Verification.Warnings
		staged = append(staged, s)
	}

	assistantID := ""
	if turn.AssistantMessageID != nil {
		assistantID = *turn.AssistantMessageID
	}
	res, err := AgentPublisher.PublishResult(ctx, PublicationInput{
		TenantID:           sc.TenantID,
		RunID:              sc.Run.ID,
		ActorID:            turn.ActorID,
		AssistantMessageID: assistantID,
		Answer:             answer,
		Artifacts:          staged,
		Outcome:            in.Outcome,
		Fence: func(fctx context.Context, fn func(tx *u.TenantRepo) error) error {
			return sc.WithLeaseTransaction(fctx, func(tx *gorm.DB) error {
				return fn(u.ForTenantTx(tx, sc.TenantID))
			})
		},
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, runner.ErrRunCancelled) || errors.Is(err, runner.ErrLeaseLost) ||
			errors.Is(err, runner.ErrRunClosed) {
			return nil, err
		}
		if errors.Is(err, ErrPublicationRefused) {
			return agentPublicationRefused(sc, repo, turn, in, err), nil
		}
		nomosAgentLog.Warnf("Publication failed (run=%s): %v", turn.RunID, err)
		return nil, runner.Retry("publication_failed", "the verified result could not be published")
	}
	nomosAgentLog.Infof("Verified result published (run=%s outcome=%s artifacts=%d citations=%d reused=%t warnings=%d)",
		turn.RunID, in.Outcome, len(res.ArtifactIDs), len(res.CitationIDs), res.Reused, warnings)

	verificationID := answer.Verification.ID
	activity := types.AgentActivityPayload{
		Dispatch: in.Dispatch, CallID: in.CallID, Tool: types.ToolSubmitResult, ProgressNote: in.ProgressNote,
		Status: types.ActivityOK,
		Object: types.AgentActivityObject{Kind: types.ActivityObjectSubmission, Label: "Résultat vérifié et publié"},
		Counts: &types.AgentActivityCounts{Warnings: intPtr(warnings), Results: intPtr(len(res.ArtifactIDs))},
	}
	return runner.Expansion{
		Output: map[string]any{"artifacts": len(res.ArtifactIDs), "citations": len(res.CitationIDs), "reused": res.Reused},
		Quiet:  true,
		Steps: []runner.StepSpec{{
			Key: agentDeliverKey(in.Decision), Kind: stepAgentDeliver, Label: "Remise de la réponse vérifiée",
			Input: agentDeliverInput{Decision: in.Decision}, DependsOn: []string{sc.Step.StepKey},
		}},
		Commit: agentCommit(sc.TenantID, func(tx *u.TenantRepo) error {
			now := time.Now().Unix()
			if err := tx.AgentTurns().Where("run_id = ? AND outcome = ''", turn.RunID).Updates(map[string]any{
				"outcome": in.Outcome, "final_verification_id": verificationID, "updated_at": now,
			}).Error; err != nil {
				return err
			}
			return emitAgentActivity(tx, turn, activity)
		}),
	}, nil
}

// agentPublicationRefused answers a publication the publisher refused. A
// refusal is deterministic — the same publication cannot pass on a retry — or
// a race the agent can resolve (the dossier moved after the gate), so it goes
// back to the agent as submit_result's observation for the next decision, as
// a gate refusal does. Refusals that keep repeating end the turn as
// no_progress; when the dossier kept moving, with an explanation that says so.
func agentPublicationRefused(sc *runner.StepCtx, repo *u.TenantRepo, turn *types.AgentTurn, in agentPublishInput, err error) any {
	n := in.Decision
	code, failure, detail := types.ToolCodePublicationRefused, "", "Publication refusée"
	message := "La publication a refusé le résultat vérifié (motif : " + clipRunes(err.Error(), 300) + ") : rien n'est publié. " +
		"Corrige la soumission — par exemple outcome « partial » si la couverture est incomplète —, vérifie de nouveau, puis soumets."
	if errors.Is(err, errPublicationScopeChanged) {
		code, failure, detail = types.ToolCodeScopeChanged, agentCodeScopeChanged, "Dossier modifié depuis la vérification"
		message = "Le dossier a changé depuis la vérification finale (pièce ajoutée, retirée ou modifiée) : le résultat n'est pas publié. " +
			"Tiens compte des pièces nouvelles si la réponse en dépend, puis soumets de nouveau : la vérification finale portera " +
			"sur le dossier tel qu'il est."
	}
	raw, _ := json.Marshal(map[string]any{"message": message})
	obs := types.ToolObservation{
		CallID: in.CallID, Tool: types.ToolSubmitResult, Status: types.ObservationError, Code: code, Retryable: true,
		Data: raw, EvidenceIDs: []string{}, ReadReceiptIDs: []string{},
	}
	out := agentGateRecord{Decision: n, Observation: &obs, Signature: agentFailureSignature(in.Call, code)}
	activity := types.AgentActivityPayload{
		Dispatch: in.Dispatch, CallID: in.CallID, Tool: types.ToolSubmitResult, ProgressNote: in.ProgressNote,
		Status: types.ActivityError, Code: code,
		Object: types.AgentActivityObject{Kind: types.ActivityObjectSubmission, Label: "Résultat non publié", Detail: detail},
	}
	emit := func(tx *u.TenantRepo) error { return emitAgentActivity(tx, turn, activity) }
	nomosAgentLog.Warnf("Publication refused, returned to the agent (run=%s decision=%d code=%s): %v", turn.RunID, n, code, err)
	if agentNoProgress(repo, turn.RunID, []string{out.Signature}) {
		out.Ended = types.AgentOutcomeNoProgress
		return agentEnd(sc, out, types.AgentOutcomeNoProgress, failure, emit)
	}
	return runner.Expansion{
		Output: out, Quiet: true, Steps: []runner.StepSpec{agentNextDecision(n+1, sc.Step.StepKey)},
		Commit: agentCommit(sc.TenantID, emit),
	}
}
