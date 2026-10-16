package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// What the agent writes into the thread: activity rows (owner decision 1) and
// the server's own words when a turn ends without a publishable result.
// Every event is inserted in the transaction that changes the state it
// describes, under the thread lock, so the cursor never skips one.

// agentEvent appends one event of the agent's run, under its thread's lock.
// A run with no thread writes a plain run event.
func agentEvent(tx *u.TenantRepo, turn *types.AgentTurn, evType string, payload map[string]any) (types.RunEvent, error) {
	runID := turn.RunID
	if turn.ConversationID != nil {
		if err := lockThread(tx, *turn.ConversationID); err != nil {
			return types.RunEvent{}, err
		}
		return threadEvent(tx, *turn.ConversationID, &runID, evType, payload)
	}
	ev := types.RunEvent{RunID: &runID, Type: evType}
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return ev, err
		}
		ev.Payload = types.JSONB(body)
	}
	return ev, tx.Create(&ev)
}

// emitAgentActivity writes one agent.activity row. Rows sharing a call id
// replace each other in the thread; the object is the call's concrete object,
// never raw arguments or reasoning.
func emitAgentActivity(tx *u.TenantRepo, turn *types.AgentTurn, p types.AgentActivityPayload) error {
	p.At = time.Now().Unix()
	if p.Object.Kind == "" {
		p.Object.Kind = activityKindFor(p.Tool)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	_, err = agentEvent(tx, turn, types.EventAgentActivity, payload)
	return err
}

// activityKindFor is the default object kind of a tool's activity row, used
// while the call runs and when the tool names none.
func activityKindFor(tool string) string {
	switch tool {
	case types.ToolListPieces:
		return types.ActivityObjectPieces
	case types.ToolReadPiece:
		return types.ActivityObjectPiece
	case types.ToolSearchPieces, types.ToolSearchLaw, types.ToolSearchCaselaw, types.ToolSearchWeb:
		return types.ActivityObjectQuery
	case types.ToolReadLaw:
		return types.ActivityObjectLaw
	case types.ToolReadDecision:
		return types.ActivityObjectDecision
	case types.ToolReadEvidence:
		return types.ActivityObjectEvidence
	case types.ToolListCompetences, types.ToolLoadCompetence:
		return types.ActivityObjectCompetence
	case types.ToolAskLawyer:
		return types.ActivityObjectQuestion
	case types.ToolWriteDocument, types.ToolEditDocument, types.ToolReadDocument:
		return types.ActivityObjectDocument
	case types.ToolCalculate:
		return types.ActivityObjectCalculation
	case types.ToolVerifyOutput:
		return types.ActivityObjectVerification
	case types.ToolReviewOutput:
		return types.ActivityObjectReview
	case types.ToolRenderDocument, types.ToolExportDossier:
		return types.ActivityObjectArtifact
	default:
		return types.ActivityObjectSubmission
	}
}

// agentRunningObject is the object of an accepted call's `running` row (owner
// decision 1): what the call is about to touch — the query searched, the
// pièce read and from where, the document worked on, the questions asked. It
// decodes the arguments as the tool does and applies the same checks before
// naming anything, so it shows only the public fields the completed row will
// show; a reference this turn cannot see leaves the generic kind alone.
// scope resolves the turn's pièces at most once per decision.
func agentRunningObject(ctx context.Context, repo *u.TenantRepo, turn *types.AgentTurn, call types.AgentToolCall,
	scope func() (PieceScopeSnapshot, bool)) (types.AgentActivityObject, *types.AgentActivityCounts) {
	obj := types.AgentActivityObject{Kind: activityKindFor(call.Name)}
	revision := func(id, key string) *types.AgentDocumentRevision {
		q := repo.AgentDocumentRevisions().Where("run_id = ?", turn.RunID)
		switch {
		case strings.TrimSpace(id) != "":
			q = q.Where("id = ?", strings.TrimSpace(id))
		case strings.TrimSpace(key) != "":
			q = q.Where("document_key = ?", strings.TrimSpace(key)).Order("revision DESC")
		default:
			return nil
		}
		var rev types.AgentDocumentRevision
		if err := q.Take(&rev).Error; err != nil {
			return nil
		}
		return &rev
	}
	docTitle := func(rev *types.AgentDocumentRevision) string {
		return truncateRunes(firstNonEmpty(rev.Title, "Document"), 120)
	}
	publicQuery := func(q string, detail string) {
		if query, err := publicQueryArg(q); err == nil {
			obj.Label, obj.Detail = query, detail
		}
	}
	raw := call.Arguments
	switch call.Name {
	case types.ToolSearchPieces:
		var a searchPiecesArgs
		if decodeArgs(raw, &a) == nil {
			if q := strings.TrimSpace(a.Query); q != "" && utf8.RuneCountInString(q) <= maxPrivateQueryRunes {
				obj.Label, obj.Detail = truncateRunes(q, 120), "Recherche dans les pièces"
			}
		}
	case types.ToolSearchLaw:
		var a searchLawArgs
		if decodeArgs(raw, &a) == nil {
			publicQuery(a.Query, "Légifrance")
		}
	case types.ToolSearchCaselaw:
		var a searchCaselawArgs
		if decodeArgs(raw, &a) == nil {
			publicQuery(a.Query, "Judilibre")
		}
	case types.ToolSearchWeb:
		var a searchWebArgs
		if decodeArgs(raw, &a) == nil {
			publicQuery(a.Query, "Sources ouvertes")
		}
	case types.ToolReadPiece:
		var a readPieceArgs
		if decodeArgs(raw, &a) != nil || (a.MaxChars != nil && (*a.MaxChars < readPieceMin || *a.MaxChars > readPieceMax)) {
			break
		}
		if snap, ok := scope(); ok {
			if p, ok := snap.Piece(strings.TrimSpace(a.PieceID)); ok {
				obj.Label, obj.Detail = pieceLabel(p), agentCursorDetail(str(a.Cursor))
			}
		}
	case types.ToolWriteDocument:
		var a writeDocumentArgs
		if decodeArgs(raw, &a) == nil {
			obj = docActivity(strings.TrimSpace(a.Title), 1)
		}
	case types.ToolEditDocument:
		var a editDocumentArgs
		if decodeArgs(raw, &a) == nil && len(a.Edits) > 0 && len(a.Edits) <= maxEditOps {
			if rev := revision(a.RevisionID, ""); rev != nil {
				obj = docActivity(rev.Title, rev.Revision)
			}
		}
	case types.ToolReadDocument:
		var a readDocumentArgs
		if decodeArgs(raw, &a) == nil && (str(a.RevisionID) == "") != (str(a.DocumentKey) == "") {
			if rev := revision(str(a.RevisionID), str(a.DocumentKey)); rev != nil {
				obj = docActivity(rev.Title, rev.Revision)
			}
		}
	case types.ToolVerifyOutput:
		var a revisionArgs
		if decodeArgs(raw, &a) == nil {
			if rev := revision(a.RevisionID, ""); rev != nil {
				obj.Label = docTitle(rev)
			}
		}
	case types.ToolReviewOutput:
		var a reviewOutputArgs
		if decodeArgs(raw, &a) == nil && utf8.RuneCountInString(str(a.Focus)) <= 300 {
			if rev := revision(a.RevisionID, ""); rev != nil {
				obj.Label = docTitle(rev)
			}
		}
	case types.ToolAskLawyer:
		if a, err := ParseAskLawyerArgs(raw, turnContract(turn)); err == nil {
			obj.Label = clipRunes(a.Questions[0].Prompt, 140)
			return obj, &types.AgentActivityCounts{Questions: intPtr(len(a.Questions))}
		}
	}
	return obj, nil
}

// agentScopeOnce resolves the turn's pièce scope the first time a running row
// needs it — only while the actor may still read the dossier, as the tools
// require — and remembers the answer for the rest of the decision.
func agentScopeOnce(ctx context.Context, repo *u.TenantRepo, turn *types.AgentTurn) func() (PieceScopeSnapshot, bool) {
	var (
		done bool
		snap PieceScopeSnapshot
		ok   bool
	)
	return func() (PieceScopeSnapshot, bool) {
		if done {
			return snap, ok
		}
		done = true
		contract := turnContract(turn)
		if contract.Scope.DossierID == "" || !agentPermissions(repo, turn).DossierRead {
			return snap, false
		}
		s, err := SnapshotPieceScope(ctx, repo, contract.Scope)
		if err != nil {
			nomosAgentLog.Warnf("Pièce scope unavailable for running rows (run=%s): %v", turn.RunID, err)
			return snap, false
		}
		snap, ok = s, true
		return snap, ok
	}
}

// agentCursorDetail is where a read starts, shown until the completed row
// names the exact range read.
func agentCursorDetail(cursor string) string {
	seg, _, _ := strings.Cut(strings.TrimSpace(cursor), "@")
	if seg == "" {
		return "depuis le début"
	}
	if n, err := strconv.Atoi(seg[1:]); err == nil && n > 0 {
		switch seg[0] {
		case 'p':
			return fmt.Sprintf("à partir de la page %d", n)
		case 's':
			return fmt.Sprintf("à partir de la feuille %d", n)
		}
	}
	return "suite de la lecture"
}

func intPtr(n int) *int { return &n }

// clipRunes shortens public text (a question shown as an activity label) on
// a rune boundary, marking the cut.
func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// shortRef is the support reference shown to the lawyer: the run id's head.
func shortRef(runID string) string {
	if len(runID) > 8 {
		return runID[:8]
	}
	return runID
}

// Failure codes a step ends a turn with, beyond the outcome itself. They
// pick the explanation and land in runs.failure_code.
const (
	agentCodeGatewayUnavailable        = "gateway_unavailable"
	agentCodeOfficialSourceUnavailable = "official_source_unavailable"
	agentCodeVerificationUnavailable   = "verification_unavailable"
	agentCodePublicationUnavailable    = "publication_unavailable"
	agentCodeToolFailed                = "tool_failed"
	agentCodeRuntimeUnavailable        = "agent_unavailable"
	agentCodeScopeChanged              = "scope_changed"
)

// agentExplanation is the server-written French text that closes a turn
// without a publishable result. It never invites an unchanged resend: when
// something can change the outcome it names that information, otherwise it
// says plainly that a resend would not help.
func agentExplanation(outcome, failureCode string, turn *types.AgentTurn) string {
	ref := shortRef(turn.RunID)
	if failureCode == agentCodeScopeChanged {
		// Not futile: the verified result lost a race with the dossier.
		return "Le dossier a été modifié pendant le travail (pièce ajoutée, retirée ou remplacée) : le résultat, vérifié " +
			"sur son état précédent, n'a pas été publié. Les résultats vérifiés sont conservés. Une fois les pièces " +
			"stabilisées, la demande pourra être relancée : elle portera alors sur le dossier tel qu'il est. Référence : " + ref + "."
	}
	switch outcome {
	case types.AgentOutcomeTurnLimit:
		return fmt.Sprintf("Nomos a atteint la limite de travail de cette demande (%d tours de décision). "+
			"Les résultats vérifiés sont conservés ; le travail reste incomplet et rien de non vérifié n'est transmis. "+
			"Renvoyer la même demande ne permettrait pas d'aller plus loin : pour aboutir, restreignez-la à une "+
			"question, à une partie du dossier ou à une liste de pièces à traiter en priorité.", turn.MaxDispatches)
	case types.AgentOutcomeDeadline:
		return fmt.Sprintf("Le temps de travail alloué à cette demande (%d minutes, hors attente de vos réponses) est écoulé. "+
			"Les résultats vérifiés sont conservés ; le travail reste incomplet et rien de non vérifié n'est transmis. "+
			"Pour aboutir, restreignez la demande à une question ou à une partie du dossier.", types.DefaultAgentActiveSeconds/60)
	case types.AgentOutcomeNoProgress:
		return "Nomos s'est arrêté : la même action a échoué trois fois sans que rien ne change. " +
			"Le travail reste incomplet et rien de non vérifié n'est transmis. Renvoyer la demande à l'identique " +
			"mènerait au même blocage ; précisez la pièce, la source ou la référence à utiliser. Référence : " + ref + "."
	case types.AgentOutcomeRefused:
		if failureCode == agentCodeVerificationUnavailable || failureCode == agentCodePublicationUnavailable {
			return "La vérification et la publication des résultats ne sont pas disponibles sur cette instance : " +
				"aucun résultat non vérifié n'est transmis. Renvoyer la demande ne changera rien tant que ce service " +
				"n'est pas rétabli. Référence à communiquer au support : " + ref + "."
		}
		return "Le résultat n'a pas passé la vérification finale : il n'est pas publié. " +
			"Renvoyer la même demande ne corrigera pas ce point. Référence : " + ref + "."
	case types.AgentOutcomeCancelled:
		return "Demande annulée. Aucun travail supplémentaire ne sera effectué ; les résultats déjà vérifiés sont conservés."
	}
	switch failureCode {
	case agentCodeGatewayUnavailable:
		return "Le service de modèles n'a pas répondu après plusieurs tentatives ; le travail est interrompu et rien de " +
			"non vérifié n'est transmis. Votre demande est conservée : elle pourra être relancée une fois le service " +
			"rétabli. Référence : " + ref + "."
	case agentCodeOfficialSourceUnavailable:
		return "La source officielle (Légifrance ou Judilibre) n'a pas répondu pendant plusieurs tentatives : les " +
			"citations juridiques du résultat n'ont pas pu être revérifiées, et rien n'est publié. Il s'agit d'une " +
			"indisponibilité de la source, non d'un problème de votre demande : elle est conservée et pourra être " +
			"relancée telle quelle une fois la source rétablie. Référence : " + ref + "."
	case agentCodeRuntimeUnavailable:
		return "L'assistant n'est pas entièrement configuré sur cette instance ; aucun travail n'a pu être mené. " +
			"Renvoyer la demande ne changera rien tant que la configuration n'est pas complétée. Référence à " +
			"communiquer au support : " + ref + "."
	}
	return "Le travail n'a pas pu être terminé. Votre demande et les résultats déjà vérifiés sont conservés. " +
		"Renvoyer la même demande ne corrigera pas ce problème. Référence à communiquer au support : " + ref + "."
}

// agentCorrection is the user message that follows a rejected dispatch with
// no executable call. It tells the controller what the server did with its
// reply — never a restatement of the task.
func agentCorrection(code string) string {
	switch code {
	case types.AgentOutcomeSubmissionRequired:
		return "<correction>\nTa réponse précédente était du texte libre : il n'est jamais transmis à l'avocat. " +
			"Poursuis le travail avec les outils, puis soumets le résultat avec submit_result.\n</correction>"
	case types.AgentOutcomeModelRefusal:
		return "<correction>\nTa réponse précédente a été un refus : aucun appel n'a été exécuté. Si une partie de la " +
			"demande ne peut pas être traitée, soumets avec submit_result un résultat qui le dit et explique pourquoi.\n</correction>"
	case types.AgentOutcomeModelTruncated:
		return "<correction>\nTa réponse précédente a été coupée par la limite de longueur : aucun appel n'a été " +
			"exécuté. Produis des appels plus courts ; un long document s'écrit par sections " +
			"(write_document puis edit_document).\n</correction>"
	}
	return "<correction>\nTa réponse précédente n'a pas pu être exécutée.\n</correction>"
}

// agentCallProblem is the guidance carried by a call refused at validation.
func agentCallProblem(code, detail string) json.RawMessage {
	var msg string
	switch code {
	case types.ToolCodeUnknownTool:
		msg = "Outil inconnu ou indisponible : " + detail + ". N'utilise que les outils proposés."
	case types.ToolCodeInvalidArguments:
		msg = "Arguments invalides : " + detail + ". Corrige l'appel selon le schéma de l'outil."
	case types.ToolCodeExclusiveCall:
		msg = "ask_lawyer et submit_result s'appellent seuls dans leur tour : aucun appel de ce tour n'a été exécuté."
	case types.ToolCodeTooManyCalls:
		msg = fmt.Sprintf("Au plus %d appels par tour : aucun appel de ce tour n'a été exécuté. Regroupe ou séquence tes actions.",
			types.MaxAgentParallelReads)
	default:
		msg = detail
	}
	raw, _ := json.Marshal(map[string]string{"message": msg})
	return raw
}
