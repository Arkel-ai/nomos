package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Delivery (§5.5): the lawyer sees only the VERIFIED answer, streamed from
// its stored revision through the thread's existing message.delta /
// message.complete events. Nothing is generated here — no model rephrases
// the verified text, which would invalidate its checks. Each chunk appends
// the content, advances the turn's delivery cursor and inserts its delta in
// one lease-fenced transaction, so a retry resumes at the committed offset
// and never repeats or skips a character.

var errAgentDeliveryMoved = errors.New("agent delivery cursor moved")

// agentDeliveryChunk is one durable delta: a half-open rune range.
type agentDeliveryChunk struct {
	Start, End int
	Text       string
}

// agentChunks cuts text deterministically, preferring a word boundary, so
// every attempt cuts at the same offsets.
func agentChunks(text string) []agentDeliveryChunk {
	runes := []rune(text)
	var out []agentDeliveryChunk
	for start := 0; start < len(runes); {
		end := start + agentDeliveryRunes
		if end >= len(runes) {
			end = len(runes)
		} else {
			for cut := end; cut > start+agentDeliveryRunes/2; cut-- {
				if runes[cut-1] == ' ' || runes[cut-1] == '\n' {
					end = cut
					break
				}
			}
		}
		out = append(out, agentDeliveryChunk{Start: start, End: end, Text: string(runes[start:end])})
		start = end
	}
	return out
}

func agentDeliverStep(ctx context.Context, sc *runner.StepCtx) (any, error) {
	repo := u.ForTenantID(ctx, sc.TenantID)
	turn, err := loadAgentTurn(repo, sc.Run.ID)
	if err != nil {
		return nil, err
	}
	if turn.AssistantMessageID == nil || turn.ConversationID == nil {
		// No thread to deliver into: the result is the published revision
		// and artifacts.
		return map[string]any{"delivered": 0}, nil
	}
	var present int64
	if err := repo.Messages().Where("id = ?", *turn.AssistantMessageID).Count(&present).Error; err != nil {
		return nil, err
	}
	if present == 0 {
		// The thread was deleted while the turn ran; the published result
		// stays with the run.
		nomosAgentLog.Infof("Thread gone, verified answer not delivered (run=%s)", turn.RunID)
		return map[string]any{"delivered": 0, "thread_deleted": true}, nil
	}
	text, verification, err := agentAnswerText(ctx, repo, turn)
	if err != nil {
		return nil, runner.Permanent("verified_answer_missing", "the published answer cannot be read back")
	}
	chunks := agentChunks(text)
	delivered := turn.DeliveredChars
	for i, ch := range chunks {
		if ch.End <= delivered {
			continue
		}
		seq := i + 1
		err := sc.WithLeaseTransaction(ctx, func(tx *gorm.DB) error {
			trepo := u.ForTenantTx(tx, sc.TenantID)
			res := trepo.AgentTurns().Where("run_id = ? AND delivered_chars = ?", turn.RunID, ch.Start).
				Updates(map[string]any{"delivered_chars": ch.End, "updated_at": time.Now().Unix()})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return errAgentDeliveryMoved
			}
			return agentAppendDelta(trepo, turn, seq, ch)
		})
		if err != nil {
			return nil, err
		}
		delivered = ch.End
	}
	nomosAgentLog.Infof("Verified answer delivered (run=%s chars=%d deltas=%d)", turn.RunID, utf8.RuneCountInString(text), len(chunks))
	return runner.Expansion{
		Output: map[string]any{"delivered": utf8.RuneCountInString(text), "deltas": len(chunks)},
		Commit: agentCommit(sc.TenantID, func(tx *u.TenantRepo) error {
			return agentCompleteMessage(tx, turn, turn.Outcome, text, verification)
		}),
	}, nil
}

// agentAppendDelta appends one chunk to the assistant message and inserts its
// message.delta, under the thread lock.
func agentAppendDelta(tx *u.TenantRepo, turn *types.AgentTurn, seq int, ch agentDeliveryChunk) error {
	if err := lockThread(tx, *turn.ConversationID); err != nil {
		return err
	}
	if err := tx.Messages().Where("id = ?", *turn.AssistantMessageID).Updates(map[string]any{
		"content":    gorm.Expr("content || ?", ch.Text),
		"updated_at": time.Now().Unix(),
	}).Error; err != nil {
		return err
	}
	_, err := agentEvent(tx, turn, types.EventMessageDelta, map[string]any{
		"message_id": *turn.AssistantMessageID,
		"seq":        seq,
		"delta":      ch.Text,
		"offset":     ch.Start,
		"end":        ch.End,
	})
	return err
}

// agentCompleteMessage closes the assistant turn with its verified answer
// and the references the thread resolves citations from. message.complete
// carries the whole text, so a viewer that missed a delta still converges.
func agentCompleteMessage(tx *u.TenantRepo, turn *types.AgentTurn, outcome, text string, verification *types.AgentVerification) error {
	if err := lockThread(tx, *turn.ConversationID); err != nil {
		return err
	}
	now := time.Now().Unix()
	updates := map[string]any{"state": types.MessageStateComplete, "agent_outcome": outcome, "updated_at": now}
	payload := map[string]any{
		"message_id":    *turn.AssistantMessageID,
		"run_id":        turn.RunID,
		"state":         types.MessageStateComplete,
		"agent_outcome": outcome,
		"chars":         utf8.RuneCountInString(text),
		"content":       text,
	}
	if verification != nil {
		updates["answer_revision_id"] = verification.DocumentRevisionID
		updates["verification_id"] = verification.ID
		payload["answer_revision_id"] = verification.DocumentRevisionID
		payload["verification_id"] = verification.ID
	}
	if err := tx.Messages().Where("id = ?", *turn.AssistantMessageID).Updates(updates).Error; err != nil {
		return err
	}
	if _, err := agentEvent(tx, turn, types.EventMessageComplete, payload); err != nil {
		return err
	}
	return tx.Conversations().Where("id = ?", *turn.ConversationID).Update("updated_at", now).Error
}

// agentPartialNotice opens an answer published as explicitly incomplete.
const agentPartialNotice = "Résultat partiel — le travail demandé n'est pas complet ; ses limites sont indiquées ci-dessous."

// agentAnswerText is the published answer as the thread receives it: the
// frozen text PublishedAnswerText rebuilds from the verified revision, its
// final report and the citation numbers bound at publication — pièce
// warnings under the block they concern, [n] markers on cited claims.
// Nothing is generated here, so a resumed delivery streams the same text.
func agentAnswerText(ctx context.Context, repo *u.TenantRepo, turn *types.AgentTurn) (string, *types.AgentVerification, error) {
	if turn.FinalVerificationID == nil || turn.AssistantMessageID == nil {
		return "", nil, errors.New("the turn has no published answer")
	}
	var v types.AgentVerification
	if err := repo.AgentVerifications().Where("run_id = ? AND id = ?", turn.RunID, *turn.FinalVerificationID).
		Take(&v).Error; err != nil {
		return "", nil, err
	}
	text, err := PublishedAnswerText(ctx, repo, *turn.AssistantMessageID)
	if err != nil {
		return "", nil, err
	}
	if turn.Outcome == types.AgentOutcomePartial {
		text = agentPartialNotice + "\n\n" + text
	}
	return strings.TrimSpace(text), &v, nil
}

// ---------------------------------------------------------------------------
// The run's terminal hook
// ---------------------------------------------------------------------------

// agentRunTerminalHook runs in the transaction that ends an agent run,
// whatever ended it. It gives the turn an outcome if it has none (a crash or
// an exhausted step is agent_failed, a cancellation is cancelled), closes
// every activity row still running, and closes the assistant message: a
// published answer still owed to the lawyer is delivered from its verified
// revision, anything else gets the server's explanation. A message already
// complete is left alone.
func agentRunTerminalHook(tx *gorm.DB, run types.Run) error {
	repo := u.ForTenantTx(tx, run.TenantID)
	var turn types.AgentTurn
	if err := repo.AgentTurns().Where("run_id = ?", run.ID).Take(&turn).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	now := time.Now().Unix()
	updates := map[string]any{"updated_at": now}
	if turn.Outcome == "" {
		turn.Outcome = types.AgentOutcomeFailed
		if run.State == types.RunStateCancelled {
			turn.Outcome = types.AgentOutcomeCancelled
		}
		updates["outcome"] = turn.Outcome
	}
	if turn.FinishedAt == nil {
		updates["finished_at"] = now
	}
	if turn.ParkedAt != nil {
		updates["parked_at"] = nil
	}
	if err := repo.AgentTurns().Where("run_id = ?", run.ID).Updates(updates).Error; err != nil {
		return err
	}
	nomosAgentLog.Infof("Agent turn finished (run=%s state=%s outcome=%s failure=%s dispatches=%d)",
		run.ID, run.State, turn.Outcome, run.FailureCode, turn.Dispatches)
	if err := agentCloseRunningActivity(repo, &turn); err != nil {
		return err
	}

	if turn.AssistantMessageID == nil || turn.ConversationID == nil {
		return nil
	}
	if err := lockThread(repo, *turn.ConversationID); err != nil {
		return err
	}
	var msg types.Message
	if err := repo.Messages().Where("id = ?", *turn.AssistantMessageID).Take(&msg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // the thread was deleted
		}
		return err
	}
	if msg.State == types.MessageStateComplete || msg.State == types.MessageStateFailed {
		return nil
	}

	published := (turn.Outcome == types.AgentOutcomeCompleted || turn.Outcome == types.AgentOutcomePartial) &&
		turn.FinalVerificationID != nil
	if published {
		text, verification, err := agentAnswerText(context.Background(), repo, &turn)
		if err == nil {
			runes := []rune(text)
			if turn.DeliveredChars < len(runes) {
				ch := agentDeliveryChunk{Start: turn.DeliveredChars, End: len(runes), Text: string(runes[turn.DeliveredChars:])}
				if err := repo.AgentTurns().Where("run_id = ?", run.ID).Update("delivered_chars", ch.End).Error; err != nil {
					return err
				}
				if err := agentAppendDelta(repo, &turn, len(agentChunks(text))+1, ch); err != nil {
					return err
				}
			}
			return agentCompleteMessage(repo, &turn, turn.Outcome, text, verification)
		}
		nomosAgentLog.Errorf("Published answer unreadable at run end (run=%s): %v", run.ID, err)
		turn.Outcome = types.AgentOutcomeFailed
		if err := repo.AgentTurns().Where("run_id = ?", run.ID).Update("outcome", turn.Outcome).Error; err != nil {
			return err
		}
	}

	explanation := agentExplanation(turn.Outcome, run.FailureCode, &turn)
	ch := agentDeliveryChunk{Start: 0, End: utf8.RuneCountInString(explanation), Text: explanation}
	if msg.Content != "" {
		ch.Text = "\n\n" + explanation
	}
	if err := agentAppendDelta(repo, &turn, 1, ch); err != nil {
		return err
	}
	if turn.Outcome != types.AgentOutcomeFailed {
		return agentCompleteMessage(repo, &turn, turn.Outcome, msg.Content+ch.Text, nil)
	}
	// A technical failure keeps the thread's failed-turn shape, with the
	// explanation as the message text.
	if err := repo.Messages().Where("id = ?", *turn.AssistantMessageID).Updates(map[string]any{
		"state": types.MessageStateFailed, "agent_outcome": turn.Outcome, "updated_at": now,
	}).Error; err != nil {
		return err
	}
	_, err := agentEvent(repo, &turn, EventMessageFailed, map[string]any{
		"message_id": *turn.AssistantMessageID, "run_id": turn.RunID, "code": run.FailureCode,
		"agent_outcome": turn.Outcome, "content": msg.Content + ch.Text,
	})
	return err
}

// agentCloseRunningActivity closes every call whose latest activity row still
// says `running` — a call in flight, or a question still waiting on the
// lawyer, when the run ended — with a row of the same call id, status error
// and code interrupted, so nothing spins on in the thread.
func agentCloseRunningActivity(tx *u.TenantRepo, turn *types.AgentTurn) error {
	var events []types.RunEvent
	if err := tx.RunEvents().Where("run_id = ? AND type = ?", turn.RunID, types.EventAgentActivity).
		Order("id").Find(&events).Error; err != nil {
		return err
	}
	latest := map[string]types.AgentActivityPayload{}
	var calls []string
	for _, ev := range events {
		var p types.AgentActivityPayload
		if json.Unmarshal(ev.Payload, &p) != nil || p.CallID == "" {
			continue
		}
		if _, seen := latest[p.CallID]; !seen {
			calls = append(calls, p.CallID)
		}
		latest[p.CallID] = p
	}
	closed := 0
	for _, id := range calls {
		p := latest[id]
		if p.Status != types.ActivityRunning {
			continue
		}
		p.Status, p.Code = types.ActivityError, types.ActivityCodeInterrupted
		if err := emitAgentActivity(tx, turn, p); err != nil {
			return err
		}
		closed++
	}
	if closed > 0 {
		nomosAgentLog.Infof("Interrupted activity closed at run end (run=%s calls=%d)", turn.RunID, closed)
	}
	return nil
}
