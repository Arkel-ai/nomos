package services

import (
	"context"
	"strings"
	"testing"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// The Judilibre loader, against the real PISTE API.
//
// It loads ONE batch and keeps it. That is deliberate: the public corpus is
// shared, the upserts are idempotent, and a test that deleted the case law it
// loaded would leave the corpus in exactly the state this loader exists to fix.
//
// What is under test is not "the HTTP call works" — the connector already has
// its own tests. It is that a loaded decision is CITABLE end to end: the right
// kind so the case-law search can find it, a label a lawyer would write, no
// validity interval, and a chunk whose quote survives the citation gate.
//
// It calls the real PISTE API, so it is excluded under NOMOS_LIVE=0; under
// NOMOS_REQUIRE_DEPENDENCIES=1 a missing database or credential fails it.

func TestJudilibreLoaderLoadsCitableDecisions(t *testing.T) {
	testmode.Live(t, "Judilibre loader (live PISTE)")
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres not reachable — corpus suite skipped")
	}
	cfg := config.Load()
	j := connectors.NewJudilibre(pisteOptions(cfg, cfg.JudilibreBaseURL))
	if !j.Configured() {
		testmode.Missing(t, "no PISTE credentials — Judilibre sync cannot run")
	}
	ctx := context.Background()

	var stats loadStats
	cursor, done, err := judilibreLoader(j)(ctx, "", &stats)
	if err != nil {
		t.Fatalf("judilibre batch: %v", err)
	}
	if stats.Decisions == 0 {
		t.Fatal("the export returned no decision for the employment scope")
	}
	if stats.Documents == 0 || stats.Versions == 0 || stats.Chunks == 0 {
		t.Fatalf("a batch of %d decisions produced %d documents, %d versions, %d chunks",
			stats.Decisions, stats.Documents, stats.Versions, stats.Chunks)
	}
	if stats.License != connectors.LicenceOuverte2 || stats.Attribution == "" {
		// Licence Ouverte 2.0 permits reuse WITH attribution, and this row is
		// the last place it can still be surfaced (§13).
		t.Fatalf("licence/attribution not recorded: %q / %q", stats.License, stats.Attribution)
	}
	if !done && cursor == "" {
		t.Fatal("an unfinished load returned no cursor to resume from")
	}
	t.Logf("batch: decisions=%d documents=%d versions=%d chunks=%d embedded=%d cursor=%q done=%t",
		stats.Decisions, stats.Documents, stats.Versions, stats.Chunks, stats.Embedded, cursor, done)

	// The rows are shaped for retrieval and for citation.
	var docs []types.LegalDocument
	if err := u.DB.Where("source = ?", types.SourceJudilibre).Limit(20).Find(&docs).Error; err != nil {
		t.Fatalf("document read-back: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("nothing was written under the judilibre source")
	}
	for _, d := range docs {
		if d.Kind != legalKindDecision {
			t.Fatalf("decision %s stored with kind %q — the case-law search filters on kind and would miss it", d.ID, d.Kind)
		}
		if !strings.HasPrefix(d.CitationLabel, "Cass.") && !strings.HasPrefix(d.CitationLabel, "CA") {
			t.Fatalf("citation label is not how a lawyer writes it: %q", d.CitationLabel)
		}
		if d.CanonicalURL == "" {
			t.Fatalf("decision %s has no public URL — a citation nobody can open", d.ID)
		}
		if d.LicenseCode != connectors.LicenceOuverte2 {
			t.Fatalf("decision %s carries licence %q", d.ID, d.LicenseCode)
		}
	}

	var versions []types.LegalVersion
	if err := u.DB.Where("legal_document_id IN ?", documentIDs(docs)).Find(&versions).Error; err != nil {
		t.Fatalf("version read-back: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("no version was written for the loaded decisions")
	}
	for _, v := range versions {
		// A decision has no validity interval: its authority is contextual.
		// A valid_to in the past would silently hide it from every search.
		if v.ValidFrom != nil || v.ValidTo != nil {
			t.Fatalf("version %s carries a validity interval (%v → %v); a decision has none",
				v.ID, v.ValidFrom, v.ValidTo)
		}
		if v.State != types.LegalVersionInForce {
			t.Fatalf("version %s is in state %q — retrieval filters on the in-force state and would skip it", v.ID, v.State)
		}
		if v.DecisionDate == nil {
			t.Fatalf("version %s has no decision date", v.ID)
		}
		if strings.TrimSpace(v.Text) == "" {
			t.Fatalf("version %s was stored without text — it can never be quoted", v.ID)
		}
	}

}

func documentIDs(docs []types.LegalDocument) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out
}
