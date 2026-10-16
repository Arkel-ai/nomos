package main

// The end-to-end harness for TestAgentVasseur (agent_vasseur_test.go): the
// matter's eighteen documents in testdata/dossier-vasseur, the runner pools at
// the configured sizes, and the read-back and polling helpers its phases use.
//
// It is REAL, all the way down: the real HTTP surface with the real middleware
// chain, the real Postgres, the real runner pools, the real gateway calls, the
// real pandoc and the real object store. It therefore COSTS MONEY and takes
// minutes, so it does not run by default — see requireE2E.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/types"
)

// The matter. `testdata/` sits beside `api/`, not inside it.
const vasseurDir = "../testdata/dossier-vasseur"

// How long each phase may take before the suite calls it a failure rather than
// waiting for ever. Ingestion is the long one: eighteen documents through
// extraction, chunking and qualification, on pools sized for one pod.
const (
	ingestDeadline = 15 * time.Minute
	pollEvery      = 2 * time.Second
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// startConfiguredRunner runs the pools at the sizes THIS deployment is
// configured with, not at a size the suite picked. Throughput is part of what
// is under test — a batch of eighteen documents behaves differently on an llm
// pool of 2 than on one of 4, and the suite must feel what production feels.
func startConfiguredRunner(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx, config.Load())
	t.Cleanup(cancel)
}

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("NOMOS_E2E") == "" {
		t.Skip("set NOMOS_E2E=1 to run the end-to-end suite (real gateway calls, minutes)")
	}
}

// vasseurFiles is the matter's documents, in bordereau order.
func vasseurFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(vasseurDir)
	if err != nil {
		t.Skipf("testdata/dossier-vasseur is not present: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.EqualFold(e.Name(), "README.md") {
			continue
		}
		out = append(out, filepath.Join(vasseurDir, e.Name()))
	}
	sort.Strings(out)
	return out
}

func listPieces(t *testing.T, r *gin.Engine, firm testsupport.Firm, dossierID string) []types.Piece {
	t.Helper()
	res := testsupport.Do(r, http.MethodGet, "/api/dossiers/"+dossierID+"/pieces?limit=200", firm.UserID, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /pieces = %d — %s", res.Code, res.Body.String())
	}
	var page struct {
		Items []types.Piece `json:"items"`
	}
	testsupport.Decode(t, res.Body.Bytes(), &page)
	return page.Items
}

func getRun(t *testing.T, r *gin.Engine, firm testsupport.Firm, runID string) types.RunDetail {
	t.Helper()
	res := testsupport.Do(r, http.MethodGet, "/api/runs/"+runID, firm.UserID, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d — %s", runID, res.Code, res.Body.String())
	}
	var detail types.RunDetail
	testsupport.Decode(t, res.Body.Bytes(), &detail)
	return detail
}

// waitFor polls until the condition holds, logging the progress line the
// condition returns so a run that stalls says WHERE it stalled.
func waitFor(t *testing.T, what string, deadline time.Duration, cond func() (bool, string)) {
	t.Helper()
	start := time.Now()
	last := ""
	for time.Since(start) < deadline {
		ok, progress := cond()
		if ok {
			t.Logf("%s — reached in %s", what, time.Since(start).Round(time.Second))
			return
		}
		if progress != last {
			t.Logf("waiting for %s: %s (%s)", what, progress, time.Since(start).Round(time.Second))
			last = progress
		}
		time.Sleep(pollEvery)
	}
	t.Fatalf("timed out after %s waiting for %s — last seen: %s", deadline, what, last)
}

func failedSteps(d types.RunDetail) string {
	var parts []string
	for _, s := range d.Steps {
		if s.State == types.StepStateFailed {
			parts = append(parts, fmt.Sprintf("%s: %s %s", s.StepKey, s.ErrorCode, s.ErrorSummary))
		}
	}
	if len(parts) == 0 {
		return "no step is marked failed"
	}
	return strings.Join(parts, "; ")
}

func artifactKinds(d types.RunDetail) []string {
	out := make([]string, 0, len(d.Artifacts))
	for _, a := range d.Artifacts {
		out = append(out, a.Kind)
	}
	return out
}
