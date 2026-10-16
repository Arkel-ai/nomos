package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Context reconstruction (§3.6). Every dispatch rebuilds its transcript from
// durable rows — the recorded decisions and the tool steps' observations —
// never from memory, so a restarted process sends the same thing.
//
// The transcript is bounded. An observation is either sent WHOLE or replaced
// by an explicit "evicted" stub that keeps its evidence ids and tells the
// controller to reread with read_evidence; nothing is silently cut. Only the
// text actually sent counts: the read receipts of whole observations are
// what the dispatch consumes once its response is accepted.

// agentContext is one dispatch's input.
type agentContext struct {
	Messages []u.ToolMessage
	Tools    []u.FunctionSpec
	Manifest agentInputManifest
	// Consumed are the read receipts of the observations sent whole.
	Consumed []string
}

// agentObservationView is what the controller sees of an observation.
type agentObservationView struct {
	CallID      string          `json:"call_id"`
	Tool        string          `json:"tool"`
	Status      string          `json:"status"`
	Code        string          `json:"code,omitempty"`
	Retryable   bool            `json:"retryable"`
	Data        json.RawMessage `json:"data,omitempty"`
	EvidenceIDs []string        `json:"evidence_ids,omitempty"`
	NextCursor  string          `json:"next_cursor,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
}

// agentStubView replaces an observation that is not sent whole.
type agentStubView struct {
	CallID      string   `json:"call_id"`
	Tool        string   `json:"tool"`
	Status      string   `json:"status"`
	Code        string   `json:"code,omitempty"`
	Evicted     bool     `json:"evicted"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	NextCursor  string   `json:"next_cursor,omitempty"`
	Note        string   `json:"note"`
}

const (
	agentEvictedNote = "Résultat retiré du contexte actif pour tenir le budget de contexte : son texte n'est plus " +
		"sous tes yeux et ne compte pas comme lu dans ce tour. Relis-le avec read_evidence (identifiants ci-dessus) " +
		"avant de t'en servir."
	agentTooLargeNote = "Résultat trop volumineux pour être transmis : il n'a pas été envoyé et ne compte pas comme " +
		"lu. Relance l'outil sur une plage plus courte (curseur) ou relis les extraits avec read_evidence."
)

// agentObsEntry is one observation placed in the transcript.
type agentObsEntry struct {
	callID   string
	full     string
	stub     string
	receipts []string
	evicted  bool
	msgIndex int
}

// BuildAgentContext reconstructs the input of decision `decision`.
func BuildAgentContext(ctx context.Context, repo *u.TenantRepo, turn *types.AgentTurn, decision int,
	defs []types.AgentToolDefinition) (agentContext, error) {
	steps, err := agentRunSteps(repo, turn.RunID)
	if err != nil {
		return agentContext{}, err
	}
	contract := turnContract(turn)

	messages := []u.ToolMessage{
		{Role: u.ToolRoleSystem, Content: nomosAgentSystemPrompt},
		{Role: u.ToolRoleUser, Content: agentContextBlock(repo, turn, contract)},
	}
	var entries []*agentObsEntry
	for d := 1; d < decision; d++ {
		var rec agentDecisionRecord
		step, ok := steps[agentDecisionKey(d)]
		if !ok || !decodeRecord(step.Output, &rec) || !rec.Recorded {
			continue
		}
		if rec.Unusable != "" || len(rec.Calls) == 0 {
			if text := strings.TrimSpace(rec.Content); text != "" {
				messages = append(messages, u.ToolMessage{Role: u.ToolRoleAssistant, Content: text})
			}
			code := rec.Rejection
			if code == "" {
				code = rec.Unusable
			}
			messages = append(messages, u.ToolMessage{Role: u.ToolRoleUser, Content: agentCorrection(code)})
			continue
		}
		assistant := u.ToolMessage{
			Role: u.ToolRoleAssistant, Content: rec.Content, ReasoningDetails: rec.ReasoningDetails,
		}
		for _, call := range rec.Calls {
			assistant.ToolCalls = append(assistant.ToolCalls, u.ToolCallWire{
				ID: call.ProviderID, Name: call.Name, Arguments: string(call.Arguments),
			})
		}
		messages = append(messages, assistant)
		for i, call := range rec.Calls {
			obs := agentObservationFor(steps, rec, d, i, call)
			entry := newObsEntry(call, obs)
			messages = append(messages, u.ToolMessage{
				Role: u.ToolRoleTool, ToolCallID: call.ProviderID, Content: entry.current(),
			})
			entry.msgIndex = len(messages) - 1
			entries = append(entries, entry)
		}
	}
	messages = append(messages, u.ToolMessage{Role: u.ToolRoleUser, Content: agentStateBlock(turn, decision)})

	// Bound the transcript: evict the OLDEST observations first, each one
	// whole, until the input fits. Only an observation its stub at least
	// halves is evicted: a small one (a revision id, a verdict) is cheaper
	// whole, and replacing it would grow the input and hide it for nothing.
	total := 0
	for _, m := range messages {
		total += agentMessageChars(m)
	}
	var evicted []string
	for _, e := range entries {
		if total <= agentContextChars {
			break
		}
		if e.evicted || utf8.RuneCountInString(e.full) < 2*utf8.RuneCountInString(e.stub) {
			continue
		}
		total -= utf8.RuneCountInString(e.full) - utf8.RuneCountInString(e.stub)
		e.evicted = true
		messages[e.msgIndex].Content = e.stub
		evicted = append(evicted, e.callID)
	}

	var consumed []string
	for _, e := range entries {
		if !e.evicted {
			consumed = append(consumed, e.receipts...)
		}
	}
	tools := make([]u.FunctionSpec, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, u.FunctionSpec{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	if len(evicted) > 0 {
		nomosAgentLog.Infof("Context bounded by eviction (run=%s decision=%d evicted=%d chars=%d)",
			turn.RunID, decision, len(evicted), total)
	}
	return agentContext{
		Messages: messages,
		Tools:    tools,
		Consumed: consumed,
		Manifest: agentInputManifest{
			PromptVersion:  agentPromptVersion,
			ToolsetVersion: agentToolsetVersion(defs),
			Messages:       len(messages),
			Observations:   len(entries),
			Evicted:        evicted,
			Chars:          total,
		},
	}, nil
}

// agentObservationFor finds what call i of decision d observed: the
// server's refusal, the ask_lawyer answers, the publisher's or the gate's
// verdict on a submission, or the tool step's own observation.
func agentObservationFor(steps map[string]types.RunStep, rec agentDecisionRecord, d, i int,
	call types.AgentToolCall) types.ToolObservation {
	if obs, ok := rec.Prefailed[i]; ok {
		return obs
	}
	switch call.Name {
	case types.ToolAskLawyer:
		var obs agentObserveRecord
		if decodeRecord(steps[agentObserveKey(d)].Output, &obs) && obs.AskObservation != nil {
			return *obs.AskObservation
		}
	case types.ToolSubmitResult:
		var pub agentGateRecord
		if decodeRecord(steps[agentPublishKey(d)].Output, &pub) && pub.Observation != nil {
			return *pub.Observation
		}
		var gate agentGateRecord
		if decodeRecord(steps[agentGateKey(d)].Output, &gate) && gate.Observation != nil {
			return *gate.Observation
		}
	}
	var tool agentToolRecord
	if decodeRecord(steps[agentToolKey(d, i)].Output, &tool) {
		return tool.Observation
	}
	raw, _ := json.Marshal(map[string]string{"message": "Observation indisponible : l'appel n'a pas abouti."})
	return types.ToolObservation{
		CallID: call.ID, Tool: call.Name, Status: types.ObservationError,
		Code: types.ToolCodeSourceUnavailable, Retryable: true, Data: raw,
	}
}

func newObsEntry(call types.AgentToolCall, obs types.ToolObservation) *agentObsEntry {
	full, _ := json.Marshal(agentObservationView{
		CallID: call.ID, Tool: call.Name, Status: obs.Status, Code: obs.Code, Retryable: obs.Retryable,
		Data: obs.Data, EvidenceIDs: obs.EvidenceIDs, NextCursor: obs.NextCursor, Truncated: obs.Truncated,
	})
	stub := func(note string) string {
		raw, _ := json.Marshal(agentStubView{
			CallID: call.ID, Tool: call.Name, Status: obs.Status, Code: obs.Code, Evicted: true,
			EvidenceIDs: obs.EvidenceIDs, NextCursor: obs.NextCursor, Note: note,
		})
		return string(raw)
	}
	e := &agentObsEntry{callID: call.ID, full: string(full), stub: stub(agentEvictedNote), receipts: obs.ReadReceiptIDs}
	if utf8.RuneCount(full) > agentObservationChars {
		// Too large to send at all: explicit, and nothing in it counts as read.
		e.stub = stub(agentTooLargeNote)
		e.full = e.stub
		e.receipts = nil
		e.evicted = true
	}
	return e
}

func (e *agentObsEntry) current() string {
	if e.evicted {
		return e.stub
	}
	return e.full
}

func agentMessageChars(m u.ToolMessage) int {
	n := utf8.RuneCountInString(m.Content)
	for _, c := range m.ToolCalls {
		n += utf8.RuneCountInString(c.Name) + utf8.RuneCountInString(c.Arguments)
	}
	return n
}

// agentContextBlock is the first user message: what the turn is about. It is
// identical on every dispatch of the turn, so the transcript prefix stays
// stable.
func agentContextBlock(repo *u.TenantRepo, turn *types.AgentTurn, contract types.AgentTaskContract) string {
	var b strings.Builder
	b.WriteString("<contexte>\n")
	fmt.Fprintf(&b, "date: %s\n", time.Unix(turn.CreatedAt, 0).Format("02/01/2006"))
	if turn.DossierID != nil {
		var d types.Dossier
		var pieces int64
		if err := repo.Dossiers().Where("id = ?", *turn.DossierID).Take(&d).Error; err == nil {
			repo.Pieces().Where("dossier_id = ?", d.ID).Count(&pieces)
			fmt.Fprintf(&b, "dossier: « %s » — %d pièce(s), toutes dans le périmètre de la demande\n", d.Title, pieces)
		}
	} else {
		b.WriteString("dossier: aucun dossier n'est lié à ce fil — les outils de pièces répondent missing_dossier\n")
	}
	if c := contract.Competence; c != nil {
		fmt.Fprintf(&b, "compétence: %s (version %d), choisie par l'avocat\n", c.Slug, c.Version)
	} else {
		b.WriteString("compétence: aucune\n")
	}
	if turn.UserMessageID != nil {
		var msg types.Message
		if err := repo.Messages().Where("id = ?", *turn.UserMessageID).Take(&msg).Error; err == nil {
			var ids []string
			if len(msg.Attachments) > 0 && json.Unmarshal(msg.Attachments, &ids) == nil && len(ids) > 0 {
				fmt.Fprintf(&b, "pièces jointes à la demande: %d\n", len(ids))
			}
		}
	}
	b.WriteString("</contexte>\n")

	if history := agentThreadHistory(repo, turn); len(history) > 0 {
		b.WriteString("<fil_precedent>\n")
		for _, m := range history {
			who := "avocat"
			if m.Role == types.MessageRoleAssistant {
				who = "nomos"
			}
			fmt.Fprintf(&b, "[%s] %s\n", who, clipRunes(m.Content, agentHistoryChars))
		}
		b.WriteString("</fil_precedent>\n")
	}

	b.WriteString("<demande>\n")
	b.WriteString(contract.Instruction)
	b.WriteString("\n</demande>")
	return b.String()
}

// agentThreadHistory is the thread before this turn: the recent turns,
// oldest first, without failed or empty messages.
func agentThreadHistory(repo *u.TenantRepo, turn *types.AgentTurn) []types.Message {
	if turn.ConversationID == nil || turn.UserMessageID == nil {
		return nil
	}
	var current types.Message
	if err := repo.Messages().Where("id = ?", *turn.UserMessageID).Take(&current).Error; err != nil {
		return nil
	}
	exclude := []string{current.ID}
	if turn.AssistantMessageID != nil {
		exclude = append(exclude, *turn.AssistantMessageID)
	}
	var recent []types.Message
	if err := repo.Messages().
		Where("conversation_id = ? AND id NOT IN ? AND created_at <= ? AND content <> '' AND state <> ?",
			*turn.ConversationID, exclude, current.CreatedAt, types.MessageStateFailed).
		Order("created_at DESC, id DESC").Limit(ThreadHistoryTurns).Find(&recent).Error; err != nil {
		nomosAgentLog.Warnf("Thread history read failed (run=%s): %v", turn.RunID, err)
		return nil
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return recent
}

// agentStateBlock is the last user message: the server-computed budgets of
// this dispatch. It changes every dispatch, so it closes the transcript.
func agentStateBlock(turn *types.AgentTurn, decision int) string {
	dispatch := turn.Dispatches + 1
	remaining := turn.MaxDispatches - dispatch
	if remaining < 0 {
		remaining = 0
	}
	reviews := types.MaxAgentReviewCalls - turn.ReviewCalls
	if reviews < 0 {
		reviews = 0
	}
	web := types.MaxAgentWebSearches - turn.WebSearches
	if web < 0 {
		web = 0
	}
	minutes := (turn.DeadlineAt - time.Now().Unix()) / 60
	if minutes < 0 {
		minutes = 0
	}
	var b strings.Builder
	b.WriteString("<etat>\n")
	fmt.Fprintf(&b, "tour de décision: %d sur %d au plus — il en restera %d après celui-ci\n", dispatch, turn.MaxDispatches, remaining)
	fmt.Fprintf(&b, "revues restantes (review_output): %d sur %d\n", reviews, types.MaxAgentReviewCalls)
	fmt.Fprintf(&b, "recherches web restantes (search_web): %d sur %d\n", web, types.MaxAgentWebSearches)
	fmt.Fprintf(&b, "temps de travail restant: environ %d minute(s)\n", minutes)
	if remaining <= 2 {
		b.WriteString("le budget de décision est presque épuisé : soumets maintenant avec submit_result le travail vérifié, " +
			"en indiquant précisément ce qui reste incomplet\n")
	}
	b.WriteString("</etat>")
	return b.String()
}
