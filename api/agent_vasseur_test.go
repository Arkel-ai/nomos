package main

// THE AGENT ON THE VASSEUR MATTER — the ReAct runtime, its tools, its gate
// and its publication, end to end.
//
// Same matter and same rules as e2e_test.go: the real HTTP surface, Postgres,
// runner pools, gateway (production models), pandoc and object store, and the
// eighteen real documents of testdata/dossier-vasseur. It costs money and
// takes minutes, so it runs only on request:
//
//	NOMOS_E2E=1 go test ./ -run TestAgentVasseur -count=1 -timeout 45m -v
//
// Two instructions are played on one ingested dossier:
//
//  1. the exhaustive chronology — the agent must read every pièce (coverage
//     from the read-receipt ledger, not from the model), publish a verified
//     chronology DOCX and answer with pièce citations, inside the 40-dispatch
//     ceiling;
//  2. a free-text request for an acte (requête CPH) with no compétence
//     picked — the agent must never draft the acte (owner decision 5); the
//     firm's built-in CPH résiliation compétence exists, so it must offer it
//     to the lawyer through ask_lawyer.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

const (
	agentChronologyInstruction = "Établis la chronologie du dossier, pièce par pièce, du contrat de travail au solde de tout compte."
	agentCPHInstruction        = "Peux tu analyser les pièces de ce dossier et me préparer un projet de requête CPH en résiliation judiciaire du contrat de travail."
	agentRunDeadline           = 30 * time.Minute
	agentAskDeadline           = 15 * time.Minute
)

func TestAgentVasseur(t *testing.T) {
	requireE2E(t)
	testsupport.Boot(t)
	if !u.LLMConfigured() {
		t.Skip("LLM gateway not configured — the agent suite needs the real thing")
	}
	if u.Blob == nil {
		t.Skip("object store not configured — the chronology cannot be published")
	}
	cfg := config.Load()
	t.Logf("controller=%s review=%s extraction=%s vision=%s", cfg.PlannerModel, cfg.DrafterModel, cfg.ExtractionModel, cfg.VisionModel)

	firm := testsupport.NewFirm(t, "vasseur-agent")
	r := testsupport.Router(t)
	startConfiguredRunner(t)

	dossier := ingestVasseur(t, r, firm)

	t.Run("chronology", func(t *testing.T) {
		convID, res := agentTurn(t, r, firm, dossier.ID, agentChronologyInstruction)
		runID := *res.RunID
		var detail types.RunDetail
		waitFor(t, "the chronology run to finish", agentRunDeadline, func() (bool, string) {
			detail = getRun(t, r, firm, runID)
			return types.TerminalRunState(detail.State), agentProgress(t, firm, runID)
		})
		turn := agentTurnRow(t, firm, runID)
		activity := agentActivity(t, firm, convID)
		logAgentRun(t, turn, activity)

		if detail.State != types.RunStateSucceeded {
			t.Fatalf("run finished %s (%s, outcome %s) — %s", detail.State, detail.FailureCode, turn.Outcome, failedSteps(detail))
		}
		if turn.Dispatches > types.MaxAgentDispatches {
			t.Fatalf("%d controller dispatches, the ceiling is %d", turn.Dispatches, types.MaxAgentDispatches)
		}

		// Coverage, from the ledger.
		var cov types.CoverageReport
		res2 := testsupport.Do(r, http.MethodGet, "/api/runs/"+runID+"/coverage", firm.UserID, nil)
		if res2.Code != http.StatusOK {
			t.Fatalf("GET /runs/:id/coverage = %d — %s", res2.Code, res2.Body.String())
		}
		testsupport.Decode(t, res2.Body.Bytes(), &cov)
		t.Logf("coverage: %d/%d pièces fully read, segments consumed %d/%d (missing %d, unreadable %d, pending %d), complete=%t",
			cov.FullyRead, cov.TotalPieces, cov.Segments.Consumed, cov.Segments.Expected, cov.Segments.Missing,
			cov.Segments.Unreadable, cov.Segments.Pending, cov.Complete)
		if cov.TotalPieces != 18 || cov.FullyRead != 18 {
			for _, p := range cov.Pieces {
				if p.State != types.CoverageComplete {
					t.Logf("  pièce %d: %s (%d/%d %s)", p.Ordinal, p.State, p.Segments.Consumed, p.Segments.Expected, p.Unit)
				}
			}
			t.Fatalf("coverage %d/%d, want 18/18", cov.FullyRead, cov.TotalPieces)
		}

		// A published chronology DOCX that downloads.
		chronology := false
		for _, a := range detail.Artifacts {
			if a.DocumentRevisionID == nil {
				continue
			}
			var rev types.AgentDocumentRevision
			if err := firm.Repo().AgentDocumentRevisions().Where("id = ?", *a.DocumentRevisionID).Take(&rev).Error; err != nil {
				t.Fatalf("artifact %s names an unknown revision: %v", a.ID, err)
			}
			if rev.Kind != types.AgentOutputChronology {
				continue
			}
			if !strings.HasSuffix(strings.ToLower(a.Name), ".docx") {
				t.Errorf("chronology artifact %q is not a .docx", a.Name)
			}
			dl := testsupport.Do(r, http.MethodGet, "/api/artifacts/"+a.ID+"/download", firm.UserID, nil)
			if dl.Code != http.StatusOK {
				t.Fatalf("GET /artifacts/%s/download = %d — %s", a.ID, dl.Code, dl.Body.String())
			}
			t.Logf("chronology published: %q (piece warnings %d, partial %t)", a.Name, a.PieceWarnings, a.Partial)
			chronology = true
		}
		if !chronology {
			t.Fatalf("no chronology DOCX was published (artifacts: %v)", artifactKinds(detail))
		}

		// The delivered answer, with pièce citations.
		msg := agentMessage(t, firm, res.AssistantMessage.ID)
		if msg.State != types.MessageStateComplete || strings.TrimSpace(msg.Content) == "" {
			t.Fatalf("assistant message %s with %d chars, want a complete delivered answer", msg.State, len([]rune(msg.Content)))
		}
		cits := agentCitations(t, r, firm, runID)
		pieceCited := 0
		byType := map[string]int{}
		for _, c := range cits {
			byType[c.SourceType]++
			if c.SourceType == types.CitationSourcePiece && c.MessageID != nil && *c.MessageID == msg.ID {
				pieceCited++
			}
		}
		t.Logf("answer: %d chars, %d citations by type %v, %d pièce citations on the answer", len([]rune(msg.Content)), len(cits), byType, pieceCited)
		if pieceCited == 0 {
			t.Fatal("the delivered answer carries no citation of source_type piece")
		}

		// Each pièce citation points where its format can be pointed at: a
		// PDF by page, a DOCX by paragraph, an XLSX by sheet and cell.
		byLocator := map[string]int{}
		for _, c := range cits {
			if c.SourceType != types.CitationSourcePiece {
				continue
			}
			byLocator[c.Locator.Kind]++
			if problem := pieceLocatorProblem(c.Filename, c.Locator); problem != "" {
				t.Errorf("pièce n° %d (%s): %s — locator %+v", c.PieceOrdinal, c.Filename, problem, c.Locator)
			}
		}
		t.Logf("pièce citation locators by kind: %v", byLocator)

		// Activity rows with their public intent line.
		notes := 0
		for _, a := range activity {
			if strings.TrimSpace(a.ProgressNote) != "" {
				notes++
			}
		}
		if notes == 0 {
			t.Fatal("no agent.activity event carries a progress_note")
		}
	})

	t.Run("cph_requete_without_competence", func(t *testing.T) {
		convID, res := agentTurn(t, r, firm, dossier.ID, agentCPHInstruction)
		runID := *res.RunID
		var detail types.RunDetail
		waitFor(t, "the agent to ask the lawyer or answer", agentAskDeadline, func() (bool, string) {
			detail = getRun(t, r, firm, runID)
			return detail.State == types.RunStateWaitingInput || types.TerminalRunState(detail.State), agentProgress(t, firm, runID)
		})
		turn := agentTurnRow(t, firm, runID)
		logAgentRun(t, turn, agentActivity(t, firm, convID))

		var actes int64
		if err := firm.Repo().AgentDocumentRevisions().Where("run_id = ? AND kind = ?", runID, types.AgentOutputActe).
			Count(&actes).Error; err != nil {
			t.Fatal(err)
		}
		if actes > 0 {
			t.Fatalf("the agent drafted %d acte revision(s) with no compétence picked", actes)
		}
		if turn.Dispatches > types.MaxAgentDispatches {
			t.Fatalf("%d controller dispatches, the ceiling is %d", turn.Dispatches, types.MaxAgentDispatches)
		}

		// The firm's library holds a CPH résiliation compétence, so the agent
		// must find it and offer it to the lawyer through ask_lawyer.
		if detail.State != types.RunStateWaitingInput {
			if detail.State == types.RunStateSucceeded {
				msg := agentMessage(t, firm, res.AssistantMessage.ID)
				t.Logf("answered without asking (%d chars, outcome %s)", len([]rune(msg.Content)), turn.Outcome)
			}
			t.Fatalf("run %s (%s, outcome %s), want it parked on a question offering the CPH compétence — %s",
				detail.State, detail.FailureCode, turn.Outcome, failedSteps(detail))
		}
		offered := false
		for _, q := range detail.Questions {
			t.Logf("question (%s, required=%t): %s — options %s", q.Kind, q.Required, q.Prompt, string(q.Options))
			offered = offered || offersCPHCompetence(q.Prompt+" "+string(q.Options))
		}
		if !offered {
			t.Fatal("no question for the lawyer offers the CPH résiliation compétence")
		}
		// The asking is what is under test; stop paying for the rest.
		cancel := testsupport.Do(r, http.MethodPost, "/api/runs/"+runID+"/cancel", firm.UserID, nil)
		if cancel.Code != http.StatusOK && cancel.Code != http.StatusAccepted {
			t.Fatalf("POST /runs/:id/cancel = %d — %s", cancel.Code, cancel.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// pieceLocatorProblem says why a pièce citation does not point where its
// file's format can be pointed at, or "" when it does.
func pieceLocatorProblem(filename string, l types.SourceLocator) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		if l.Kind != types.LocatorPDF || l.Page < 1 {
			return "a PDF located without its page"
		}
	case strings.HasSuffix(lower, ".docx"):
		if l.Kind != types.LocatorDOCX || l.Paragraph < 1 || l.Page != 0 {
			return "a DOCX not located by paragraph"
		}
	case strings.HasSuffix(lower, ".xlsx"):
		if l.Kind != types.LocatorXLSX || l.Sheet == "" || l.Cell == "" || l.Page != 0 {
			return "an XLSX not located by sheet and cell"
		}
	default:
		return ""
	}
	if l.End <= l.Start {
		return "an empty range"
	}
	return ""
}

// offersCPHCompetence: the text names the built-in CPH résiliation
// compétence, by slug or by name.
func offersCPHCompetence(text string) bool {
	folded := strings.NewReplacer("é", "e", "è", "e", "ê", "e", "É", "e").Replace(strings.ToLower(text))
	return strings.Contains(folded, "requete-cph-resiliation-judiciaire") ||
		(strings.Contains(folded, "resiliation judiciaire") && strings.Contains(folded, "cph"))
}

// ingestVasseur creates the dossier and versés the eighteen pièces, then waits
// until ingestion has settled every one of them.
func ingestVasseur(t *testing.T, r *gin.Engine, firm testsupport.Firm) types.Dossier {
	t.Helper()
	var dossier types.Dossier
	res := testsupport.Do(r, http.MethodPost, "/api/dossiers", firm.UserID, types.DossierCreateRequest{
		Title: "Vasseur c/ Transports Delaunay", ClientReference: "2025-0142", MatterType: "travail",
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /dossiers = %d — %s", res.Code, res.Body.String())
	}
	testsupport.Decode(t, res.Body.Bytes(), &dossier)

	files := vasseurFiles(t)
	if len(files) != 18 {
		t.Fatalf("testdata/dossier-vasseur holds %d documents, want 18", len(files))
	}
	res, err := testsupport.Upload(r, "/api/dossiers/"+dossier.ID+"/pieces", firm.UserID, files)
	if err != nil {
		t.Fatalf("building the upload: %v", err)
	}
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /pieces = %d — %s", res.Code, res.Body.String())
	}
	var upload types.PieceUploadResponse
	testsupport.Decode(t, res.Body.Bytes(), &upload)
	if len(upload.Rejected) > 0 || len(upload.Accepted) != len(files) {
		t.Fatalf("upload accepted %d and rejected %d of %d documents", len(upload.Accepted), len(upload.Rejected), len(files))
	}
	waitFor(t, "ingestion of every pièce", ingestDeadline, func() (bool, string) {
		done, failed := 0, 0
		for _, p := range listPieces(t, r, firm, dossier.ID) {
			switch p.ClassificationState {
			case types.PieceStateProposed, types.PieceStateApproved:
				done++
			case types.PieceStateFailed:
				failed++
			}
		}
		if failed > 0 {
			t.Fatalf("%d pièce(s) failed ingestion", failed)
		}
		return done == len(files), fmt.Sprintf("%d/%d ingested", done, len(files))
	})
	return dossier
}

// agentTurn opens a thread on the dossier and sends one instruction; the
// response must show the turn entered the agent runtime.
func agentTurn(t *testing.T, r *gin.Engine, firm testsupport.Firm, dossierID, instruction string) (string, types.MessageCreateResponse) {
	t.Helper()
	res := testsupport.Do(r, http.MethodPost, "/api/conversations", firm.UserID, types.ConversationCreateRequest{DossierID: &dossierID})
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /conversations = %d — %s", res.Code, res.Body.String())
	}
	var conv types.Conversation
	testsupport.Decode(t, res.Body.Bytes(), &conv)
	res = testsupport.Do(r, http.MethodPost, "/api/conversations/"+conv.ID+"/messages", firm.UserID,
		types.MessageCreateRequest{Content: instruction})
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /messages = %d — %s", res.Code, res.Body.String())
	}
	var out types.MessageCreateResponse
	testsupport.Decode(t, res.Body.Bytes(), &out)
	if out.Agent == nil || out.RunID == nil {
		t.Fatalf("the turn did not enter the agent runtime: %s", res.Body.String())
	}
	return conv.ID, out
}

func agentTurnRow(t *testing.T, firm testsupport.Firm, runID string) types.AgentTurn {
	t.Helper()
	var turn types.AgentTurn
	if err := firm.Repo().AgentTurns().Where("run_id = ?", runID).Take(&turn).Error; err != nil {
		t.Fatalf("agent turn: %v", err)
	}
	return turn
}

func agentMessage(t *testing.T, firm testsupport.Firm, id string) types.Message {
	t.Helper()
	var msg types.Message
	if err := firm.Repo().Messages().Where("id = ?", id).Take(&msg).Error; err != nil {
		t.Fatalf("message: %v", err)
	}
	return msg
}

// agentActivity is the thread's agent.activity rows, in commit order.
func agentActivity(t *testing.T, firm testsupport.Firm, convID string) []types.AgentActivityPayload {
	t.Helper()
	var events []types.RunEvent
	if err := firm.Repo().RunEvents().Where("conversation_id = ? AND type = ?", convID, types.EventAgentActivity).
		Order("id ASC").Find(&events).Error; err != nil {
		t.Fatalf("activity events: %v", err)
	}
	out := make([]types.AgentActivityPayload, 0, len(events))
	for _, ev := range events {
		var p types.AgentActivityPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("activity payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func agentCitations(t *testing.T, r *gin.Engine, firm testsupport.Firm, runID string) []types.AgentCitation {
	t.Helper()
	var all []types.AgentCitation
	after := ""
	for {
		res := testsupport.Do(r, http.MethodGet, "/api/runs/"+runID+"/citations?limit=100"+after, firm.UserID, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("GET /runs/:id/citations = %d — %s", res.Code, res.Body.String())
		}
		var page types.AgentCitationList
		testsupport.Decode(t, res.Body.Bytes(), &page)
		all = append(all, page.Items...)
		if page.NextAfter == nil {
			return all
		}
		after = fmt.Sprintf("&after=%d", *page.NextAfter)
	}
}

// agentProgress is the polling line: dispatches so far and the last call.
func agentProgress(t *testing.T, firm testsupport.Firm, runID string) string {
	t.Helper()
	var turn types.AgentTurn
	if err := firm.Repo().AgentTurns().Where("run_id = ?", runID).Take(&turn).Error; err != nil {
		return "no turn yet"
	}
	return fmt.Sprintf("dispatch %d/%d outcome=%q", turn.Dispatches, turn.MaxDispatches, turn.Outcome)
}

// logAgentRun prints the numbers the report needs: dispatches, outcome and
// the tool sequence as the thread shows it (final rows only).
func logAgentRun(t *testing.T, turn types.AgentTurn, activity []types.AgentActivityPayload) {
	t.Helper()
	t.Logf("turn: dispatches=%d/%d outcome=%s reviews=%d web=%d", turn.Dispatches, turn.MaxDispatches, turn.Outcome,
		turn.ReviewCalls, turn.WebSearches)
	final := map[string]types.AgentActivityPayload{}
	var order []string
	for _, a := range activity {
		key := a.CallID + "|" + a.Tool
		if _, ok := final[key]; !ok {
			order = append(order, key)
		}
		final[key] = a
	}
	tools := map[string]int{}
	for _, key := range order {
		a := final[key]
		tools[a.Tool]++
		detail := a.Object.Detail
		if detail != "" {
			detail = " · " + detail
		}
		t.Logf("  d%-2d %-16s %-7s %s%s — %s", a.Dispatch, a.Tool, a.Status, a.Object.Kind, detail, a.ProgressNote)
	}
	t.Logf("tool calls: %v", tools)
}
