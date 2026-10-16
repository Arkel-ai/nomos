package main

// THE SAFETY SUITE (docs/PLAN.md §6.1, §6.4, §13).
//
// Durability is about not losing work. This file is about not doing harm.
// Three properties, in descending order of how badly they end:
//
//  1. CITATION RESOLVE-OR-REFUSE (§13). A fabricated citation in a filed
//     pleading prejudices the client, misleads the court, creates liability
//     and breaches competence, prudence, independence and diligence. The
//     product's answer is a REFUSAL, not a score, and a refusal nobody has
//     tried to slip past is a refusal nobody can trust.
//  2. CROSS-TENANT ISOLATION (§6.1). Tenant scoping here is a
//     `WHERE tenant_id = ?` a human has to remember to write — it is not
//     Postgres RLS, and it is one missed clause away from a breach of secret
//     professionnel. api/tenant_test.go attacks dossiers and pieces; this
//     file attacks everything else a run leaves behind: events, artifacts,
//     citations, piece_chunks and search.
//  3. NO JUDGE ANALYTICS (§13). COJ art. L. 111-13 criminally sanctions
//     reusing MAGISTRATES' identity data to evaluate, analyse, compare or
//     predict their professional practices. This is not a feature we decided
//     against; it is one we are not allowed to have. So there is a guard test
//     that greps for it creeping in.
//
// The attacker in §2 always holds VALID uuids belonging to the other firm.
// Guessing ids was never the threat model — a shared bucket, a shared
// database and a copied link are.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// ===========================================================================
// 1. CITATION RESOLVE-OR-REFUSE (§6.4, §13)
// ===========================================================================

func citHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestCitationTemporalWindowExcludesOutOfForceVersions proves the temporal
// half-open interval of §6.4 at the only place it is currently enforced:
// retrieval. The applicable rule is the one in force at the date of the
// FACTS, so a version whose validity ended before the matter date must not be
// retrievable at all — and therefore can never become evidence a citation
// could rest on.
// safetyFixtureSource keeps this test's two versions out of the real corpus's
// way, in both directions.
const safetyFixtureSource = "safety_fixture"

func TestCitationTemporalWindowExcludesOutOfForceVersions(t *testing.T) {
	testsupport.Boot(t)

	docID := uuid.NewString()
	oldID, newID := uuid.NewString(), uuid.NewString()
	cut := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	oldFrom := time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

	t.Cleanup(func() {
		u.DB.Exec(`DELETE FROM legal_chunks WHERE legal_version_id IN (?, ?)`, oldID, newID)
		u.DB.Exec(`DELETE FROM legal_versions WHERE legal_document_id = ?`, docID)
		u.DB.Exec(`DELETE FROM legal_documents WHERE id = ?`, docID)
	})

	// Its own source, so the retrieval below can be scoped to these two
	// versions. What is under test is the temporal WINDOW, not whether a
	// fixture out-ranks a real corpus: once Judilibre was loaded, the same two
	// chunks stopped reaching the top of a global lexical search and the test
	// failed for a reason that had nothing to do with validity intervals.
	if err := u.DB.Create(&types.LegalDocument{
		ID: docID, Source: safetyFixtureSource, SourceID: "SAFETY-" + docID[:8],
		Kind: "article", Jurisdiction: "FR",
		CitationLabel: "C. trav., art. L. 9999-1 (fixture)", LicenseCode: "LO-2.0",
	}).Error; err != nil {
		t.Fatalf("seeding the legal document: %v", err)
	}
	versions := []types.LegalVersion{
		{ID: oldID, LegalDocumentID: docID, SourceVersion: "v-ancienne",
			Text:       "Rédaction ancienne : le délai de prescription quinquennale sécurisation est de cinq ans.",
			TextSHA256: citHash("ancienne"), ValidFrom: &oldFrom, ValidTo: &cut, State: types.LegalVersionInForce},
		{ID: newID, LegalDocumentID: docID, SourceVersion: "v-actuelle",
			Text:       "Rédaction actuelle : le délai de prescription quinquennale sécurisation est de deux ans.",
			TextSHA256: citHash("actuelle"), ValidFrom: &cut, State: types.LegalVersionInForce},
	}
	if err := u.DB.Create(&versions).Error; err != nil {
		t.Fatalf("seeding the legal versions: %v", err)
	}
	for _, v := range versions {
		ch := types.LegalChunk{
			LegalVersionID: v.ID, Ordinal: 0, Text: v.Text,
			TextSHA256: citHash(v.Text), EmbeddingModel: "safety-fixture", TokenCount: 20,
		}
		if err := u.DB.Create(&ch).Error; err != nil {
			t.Fatalf("seeding the chunk: %v", err)
		}
		// search_vector is written by application DML (§6.2), so the fixture
		// writes it the same way the loader does.
		if err := u.DB.Exec(
			`UPDATE legal_chunks SET search_vector = to_tsvector('french', unaccent(text)) WHERE id = ?`,
			ch.ID).Error; err != nil {
			t.Fatalf("writing search_vector: %v", err)
		}
	}

	query := "prescription quinquennale sécurisation"
	ask := func(asOf int64) []services.Hit {
		t.Helper()
		opts := services.DefaultRetrievalOptions()
		opts.Strategy = services.StrategyFTS // lexical arm only: no gateway needed
		opts.AsOfDate = asOf
		opts.Sources = []string{safetyFixtureSource}
		res, err := services.Retrieve(context.Background(), services.RetrievalRequest{
			Query: query, Corpus: types.CorpusPublic, Options: opts,
		})
		if err != nil {
			t.Fatalf("Retrieve(as_of=%d): %v", asOf, err)
		}
		var mine []services.Hit
		for _, h := range res.Hits {
			if h.LegalVersionID == oldID || h.LegalVersionID == newID {
				mine = append(mine, h)
			}
		}
		return mine
	}

	// A dispute about facts from 2012 must see the OLD wording and only it.
	facts2012 := time.Date(2012, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	hits := ask(facts2012)
	if len(hits) != 1 || hits[0].LegalVersionID != oldID {
		t.Fatalf("as_of 2012 returned %d hits (%v), want only the version in force then", len(hits), versionIDsOf(hits))
	}

	// A dispute about facts from 2022 must see the CURRENT wording and only it.
	facts2022 := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	hits = ask(facts2022)
	if len(hits) != 1 || hits[0].LegalVersionID != newID {
		t.Fatalf("as_of 2022 returned %d hits (%v), want only the current version", len(hits), versionIDsOf(hits))
	}

	// The interval is HALF-OPEN: valid_from <= d AND d < valid_to. The exact
	// cut-over second belongs to the new version, never to both.
	hits = ask(cut)
	if len(hits) != 1 || hits[0].LegalVersionID != newID {
		t.Fatalf("as_of exactly at the cut-over returned %v, want only the new version — the interval must be half-open", versionIDsOf(hits))
	}
	t.Logf("temporal window holds: 2012→ancienne, 2022→actuelle, cut-over→actuelle")
}

func versionIDsOf(hits []services.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.LegalVersionID)
	}
	return out
}

// The three verification states are never collapsed. `lawyer_verified` is a
// human act and no automated path may write it — not the drafting step, not
// export-time revalidation, not a reconciliation job.
func TestCitationLawyerVerifiedIsNeverWrittenByCode(t *testing.T) {
	root := repoRoot(t)
	hits := grepTree(t, filepath.Join(root, "api"), regexp.MustCompile(`CitationLawyerVerified`),
		func(path string) bool { return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") })

	for _, h := range hits {
		// Reading it (to protect a verified row) is fine. Assigning it is not.
		line := strings.TrimSpace(h.line)
		assigns := strings.Contains(line, "= types.CitationLawyerVerified") ||
			strings.Contains(line, "VerificationState: types.CitationLawyerVerified") ||
			strings.Contains(line, `"verification_state", types.CitationLawyerVerified`)
		if assigns {
			t.Errorf("%s:%d writes lawyer_verified from code — that state means a human took responsibility:\n\t%s",
				h.path, h.n, line)
		}
	}
	if len(hits) == 0 {
		t.Fatal("CitationLawyerVerified is referenced nowhere — the three-state distinction is not implemented")
	}
	t.Logf("lawyer_verified appears in %d non-test line(s), none of them an assignment", len(hits))
}

// ===========================================================================
// 2. CROSS-TENANT ISOLATION FOR EVERYTHING A RUN LEAVES BEHIND (§6.1)
// ===========================================================================

// safetyTenants is the two-firm attack setup: firm A does real work, firm B
// holds firm A's real uuids and tries to read all of it.
type safetyTenants struct {
	router  *gin.Engine
	a, b    testsupport.Firm
	dosA    string
	dosB    string
	runA    string
	stepA   string
	artA    string
	citA    string
	chunkA  uint64
	secretA string
}

func setupSafetyTenants(t *testing.T) *safetyTenants {
	t.Helper()
	testsupport.Boot(t)

	s := &safetyTenants{
		router:  testsupport.Router(t),
		a:       testsupport.NewFirm(t, "victim"),
		b:       testsupport.NewFirm(t, "attacker"),
		secretA: "SECRET-" + uuid.NewString(),
	}
	s.dosA = seedSafetyDossier(t, s.a, "Prud'hommes — "+s.secretA)
	s.dosB = seedSafetyDossier(t, s.b, "Litige — firme B")

	// A real run, with its real events, in firm A.
	run, err := runner.Enqueue(context.Background(), runner.RunSpec{
		TenantID: s.a.ID, DossierID: s.dosA, RequestedBy: s.a.UserID,
		WorkflowType: types.WorkflowDossierOrganizeV1,
		Instruction:  "organiser le dossier " + s.secretA,
		Steps:        []runner.StepSpec{{Key: "plan", Kind: kindHold, Label: "Plan"}},
	})
	if err != nil {
		t.Fatalf("seeding firm A's run: %v", err)
	}
	// Freeze it: this suite is about reads, and a claimer would only add noise.
	if err := u.DB.Exec(
		`UPDATE runs SET cancel_requested_at = EXTRACT(EPOCH FROM now())::bigint WHERE id = ?`,
		run.ID).Error; err != nil {
		t.Fatalf("freezing firm A's run: %v", err)
	}
	s.runA = run.ID
	var step types.RunStep
	if err := u.DB.Where("run_id = ?", run.ID).Take(&step).Error; err != nil {
		t.Fatalf("firm A's step: %v", err)
	}
	s.stepA = step.ID

	repoA := s.a.Repo()

	// An artifact (with its asset), a citation and a piece chunk — the four
	// tables §6.1 says must never cross, plus the search path below.
	assetID := uuid.NewString()
	if err := repoA.Create(&types.Asset{
		ID: assetID, S3Key: u.DossierKey(s.a.ID, s.dosA, "bordereau.docx"),
		Kind: types.AssetKindArtifact, Mime: "application/octet-stream",
		Size: 10, SHA256: strings.Repeat("e", 64), CreatedBy: s.a.UserID,
	}); err != nil {
		t.Fatalf("seeding firm A's asset: %v", err)
	}
	art := types.Artifact{
		ID: uuid.NewString(), RunID: s.runA, DossierID: s.dosA,
		Kind: types.ArtifactKindBordereauDocx, AssetID: assetID,
		Name: "bordereau-" + s.secretA + ".docx", CharCount: 10,
	}
	if err := repoA.Create(&art); err != nil {
		t.Fatalf("seeding firm A's artifact: %v", err)
	}
	s.artA = art.ID

	cit := types.Citation{
		ID: uuid.NewString(), RunID: s.runA, RunStepID: &s.stepA,
		ClaimKey: "bloc#000", SourceType: types.CitationSourcePiece,
		PieceID: &s.dosA, Quote: s.secretA + " — extrait de la pièce",
		SourceSHA256: citHash(s.secretA), VerificationState: types.CitationRetrieved,
	}
	if err := repoA.Create(&cit); err != nil {
		t.Fatalf("seeding firm A's citation: %v", err)
	}
	s.citA = cit.ID

	chunk := types.PieceChunk{
		DossierID: s.dosA, PieceID: uuid.NewString(), Ordinal: 0,
		Text: "Extrait confidentiel " + s.secretA, TextSHA256: citHash(s.secretA),
		EmbeddingModel: "safety-fixture", TokenCount: 5,
	}
	if err := repoA.Create(&chunk); err != nil {
		t.Fatalf("seeding firm A's piece chunk: %v", err)
	}
	s.chunkA = chunk.ID
	return s
}

func seedSafetyDossier(t *testing.T, f testsupport.Firm, title string) string {
	t.Helper()
	d := types.Dossier{
		ID: uuid.NewString(), Title: title, State: types.DossierStateActive, CreatedBy: f.UserID,
	}
	if err := f.Repo().Create(&d); err != nil {
		t.Fatalf("seeding dossier for %s: %v", f.ID, err)
	}
	return d.ID
}

// TestCrossTenantRunSurfaceRefusesValidForeignIDs is the attack: firm B, using
// firm A's REAL uuids, tries every read the run surface offers.
func TestCrossTenantRunSurfaceRefusesValidForeignIDs(t *testing.T) {
	s := setupSafetyTenants(t)

	attacks := []struct {
		name, method, path string
		body               any
	}{
		{"run detail", http.MethodGet, "/api/runs/" + s.runA, nil},
		{"run events cursor", http.MethodGet, "/api/runs/" + s.runA + "/events?after=0&limit=500", nil},
		{"run artifacts", http.MethodGet, "/api/runs/" + s.runA + "/artifacts", nil},
		{"artifact download", http.MethodGet, "/api/artifacts/" + s.artA + "/download", nil},
		{"answer another firm's question", http.MethodPost, "/api/runs/" + s.runA + "/answers",
			map[string]any{"answers": []map[string]any{{"question_key": "nature", "values": []string{"prudhommes"}}}}},
		{"cancel another firm's run", http.MethodPost, "/api/runs/" + s.runA + "/cancel", nil},
		{"private search in another firm's dossier", http.MethodPost, "/api/search",
			map[string]any{"query": "licenciement", "corpus": types.CorpusPrivate, "dossier_id": s.dosA}},
	}

	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			w := testsupport.Do(s.router, a.method, a.path, s.b.UserID, a.body)
			testsupport.AssertDenied(t, w.Code, a.name)
			assertNoSecret(t, w.Body.String(), s)
			t.Logf("%s → %d", a.name, w.Code)
		})
	}

	// A suite that denies everything proves nothing. Firm B's own reads must
	// still work, and must contain only firm B's rows.
	t.Run("the victim's own surface still works and is not empty", func(t *testing.T) {
		w := testsupport.Do(s.router, http.MethodGet, "/api/runs/"+s.runA, s.a.UserID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("firm A cannot read its OWN run: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), s.secretA) {
			t.Fatal("firm A's own run detail does not carry its own data — the test is not proving isolation, it is proving emptiness")
		}
	})
}

// The cursor is the one thing a thread polls continuously, so a leak here
// leaks continuously. It must be scoped by tenant AND by run, and it must not
// hand a caller another firm's rows even when the caller asks from position 0.
func TestCrossTenantRunEventsCursorNeverCrosses(t *testing.T) {
	s := setupSafetyTenants(t)

	// Firm A's run really has events to leak.
	if n := testsupport.Count(t, "run_events", "tenant_id = ? AND run_id = ?", s.a.ID, s.runA); n == 0 {
		t.Fatal("firm A's run has no events — nothing to attack")
	}

	// Repository level: even with a valid foreign run id, the predicate wins.
	repoB := s.b.Repo()
	var events []types.RunEvent
	if err := repoB.RunEvents().Where("run_id = ?", s.runA).Find(&events).Error; err != nil {
		t.Fatalf("scoped event read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("the tenant-scoped repository returned %d of firm A's events to firm B", len(events))
	}

	// And a cursor that walks the whole table from 0 sees only its own rows.
	var all []types.RunEvent
	if err := repoB.RunEvents().Where("id > ?", 0).Order("id").Limit(500).Find(&all).Error; err != nil {
		t.Fatalf("cursor walk: %v", err)
	}
	for _, e := range all {
		if e.TenantID != s.b.ID {
			t.Fatalf("a cursor walk returned an event from tenant %s", e.TenantID)
		}
	}
	t.Logf("firm B's cursor sees %d events, none of them firm A's", len(all))
}

// Artifacts, citations and piece_chunks are read through the repository by
// the runner and the export path, where there is no gin.Context to carry a
// tenant. ForTenantID is the only door, and it must be locked in both
// directions.
func TestCrossTenantRepositoryRefusesArtifactsCitationsAndChunks(t *testing.T) {
	s := setupSafetyTenants(t)
	repoB := s.b.Repo()
	repoA := s.a.Repo()

	checks := []struct {
		name  string
		count func(*u.TenantRepo) int64
	}{
		{"artifacts by valid foreign id", func(r *u.TenantRepo) int64 {
			var n int64
			r.Artifacts().Where("id = ?", s.artA).Count(&n)
			return n
		}},
		{"artifacts by valid foreign run id", func(r *u.TenantRepo) int64 {
			var n int64
			r.Artifacts().Where("run_id = ?", s.runA).Count(&n)
			return n
		}},
		{"citations by valid foreign id", func(r *u.TenantRepo) int64 {
			var n int64
			r.Citations().Where("id = ?", s.citA).Count(&n)
			return n
		}},
		{"citations by valid foreign run id", func(r *u.TenantRepo) int64 {
			var n int64
			r.Citations().Where("run_id = ?", s.runA).Count(&n)
			return n
		}},
		{"piece_chunks by valid foreign dossier id", func(r *u.TenantRepo) int64 {
			var n int64
			r.PieceChunks().Where("dossier_id = ?", s.dosA).Count(&n)
			return n
		}},
		{"piece_chunks by valid foreign chunk id", func(r *u.TenantRepo) int64 {
			var n int64
			r.PieceChunks().Where("id = ?", s.chunkA).Count(&n)
			return n
		}},
		{"run_steps by valid foreign id", func(r *u.TenantRepo) int64 {
			var n int64
			r.RunSteps().Where("id = ?", s.stepA).Count(&n)
			return n
		}},
		{"step_attempts by valid foreign step id", func(r *u.TenantRepo) int64 {
			var n int64
			r.StepAttempts().Where("run_step_id = ?", s.stepA).Count(&n)
			return n
		}},
		{"llm_calls by valid foreign run id", func(r *u.TenantRepo) int64 {
			var n int64
			r.LLMCalls().Where("run_id = ?", s.runA).Count(&n)
			return n
		}},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if got := c.count(repoB); got != 0 {
				t.Fatalf("firm B read %d of firm A's rows: %s", got, c.name)
			}
		})
	}

	// Both directions: A must not be able to read B either, and A must still
	// be able to read its own rows (otherwise the predicate is just broken).
	if n := func() int64 {
		var n int64
		repoA.Artifacts().Where("id = ?", s.artA).Count(&n)
		return n
	}(); n != 1 {
		t.Fatalf("firm A cannot read its OWN artifact (%d rows) — the predicate is broken, not strict", n)
	}

	// RunStepDeps is the one private table with no tenant column of its own.
	// It is reachable only through run ids the caller already read, and an
	// empty list must match nothing rather than everything.
	var deps int64
	repoB.RunStepDeps(nil).Count(&deps)
	if deps != 0 {
		t.Fatalf("RunStepDeps(nil) matched %d rows — an empty scope must match nothing", deps)
	}
}

// The HTTP route refuses a private search into another firm's dossier
// (above). This is the same attack one layer down, where there is no route to
// refuse it: services.Retrieve is called directly by the drafting and
// note-de-synthèse steps, which run in the RUNNER, where there is no
// gin.Context and no middleware — only the repository the step was handed.
//
// §6.1 is explicit that this is the weak spot: `WHERE tenant_id = ?` is not
// RLS, and private retrieval is the one query that reads document TEXT rather
// than ids. A missed predicate here does not leak a row count, it leaks the
// contents of another firm's dossier into a generated pleading.
func TestCrossTenantPrivateRetrievalReturnsNothing(t *testing.T) {
	s := setupSafetyTenants(t)

	// search_vector is written by application DML (§6.2), so the fixture
	// writes it the same way the chunker does.
	if err := u.DB.Exec(
		`UPDATE piece_chunks SET search_vector = to_tsvector('french', unaccent(text)) WHERE id = ?`,
		s.chunkA).Error; err != nil {
		t.Fatalf("writing search_vector: %v", err)
	}

	ask := func(repo *u.TenantRepo, dossierID, corpus string) []services.Hit {
		t.Helper()
		opts := services.DefaultRetrievalOptions()
		opts.Strategy = services.StrategyFTS // lexical arm only: no gateway needed
		res, err := services.Retrieve(context.Background(), services.RetrievalRequest{
			Query: "confidentiel", Corpus: corpus, Repo: repo,
			DossierID: dossierID, Options: opts,
		})
		if err != nil {
			t.Fatalf("Retrieve(corpus=%s): %v", corpus, err)
		}
		return res.Hits
	}

	// The victim really can retrieve its own chunk — otherwise this test
	// proves emptiness, not isolation.
	own := ask(s.a.Repo(), s.dosA, types.CorpusPrivate)
	found := false
	for _, h := range own {
		if h.ChunkID == s.chunkA {
			found = true
			if !strings.Contains(h.Text, s.secretA) {
				t.Fatal("firm A's own hit does not carry its own text")
			}
		}
	}
	if !found {
		t.Fatalf("firm A retrieved %d private hits, none of them its own chunk — there is nothing here to leak", len(own))
	}

	// The attack: firm B's repository, firm A's REAL dossier id.
	for _, corpus := range []string{types.CorpusPrivate, types.CorpusBoth} {
		hits := ask(s.b.Repo(), s.dosA, corpus)
		for _, h := range hits {
			if h.ChunkID == s.chunkA {
				t.Fatalf("corpus=%s: firm B retrieved firm A's private chunk %d", corpus, h.ChunkID)
			}
			if strings.Contains(h.Text, s.secretA) {
				t.Fatalf("corpus=%s: firm B retrieved text belonging to firm A", corpus)
			}
			if h.Corpus == types.CorpusPrivate {
				t.Fatalf("corpus=%s: firm B got a private hit from a dossier it does not own", corpus)
			}
		}
	}
	t.Logf("firm A retrieves its own %d private hit(s); firm B retrieves none of them with A's real dossier id", len(own))
}

// A repository is not allowed to write into another tenant either. Create()
// stamps tenant_id by reflection precisely so a caller cannot do this by
// mistake — the row a bug writes under the wrong tenant is the same breach as
// the read it forgot to scope.
func TestCrossTenantWriteIsRestamped(t *testing.T) {
	s := setupSafetyTenants(t)

	art := types.Artifact{
		ID: uuid.NewString(), TenantID: s.a.ID, // the attacker's attempt
		RunID: s.runA, DossierID: s.dosA, Kind: types.ArtifactKindNoteDocx,
		AssetID: uuid.NewString(), Name: "smuggled.docx",
	}
	if err := s.b.Repo().Create(&art); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if art.TenantID != s.b.ID {
		t.Fatalf("the row kept tenant_id=%q — Create must restamp it to the repository's tenant", art.TenantID)
	}
	if n := testsupport.Count(t, "artifacts", "id = ? AND tenant_id = ?", art.ID, s.b.ID); n != 1 {
		t.Fatal("the row did not land in the writer's own tenant")
	}
	if n := testsupport.Count(t, "artifacts", "id = ? AND tenant_id = ?", art.ID, s.a.ID); n != 0 {
		t.Fatal("a row was written into another firm's tenant")
	}
}

// assertNoSecret is the leak check: no response to firm B may contain firm A's
// identifiers or the marker planted in its dossier title, artifact name,
// instruction and citation quote.
func assertNoSecret(t *testing.T, body string, s *safetyTenants) {
	t.Helper()
	for label, needle := range map[string]string{
		"firm A's marker":      s.secretA,
		"firm A's dossier id":  s.dosA,
		"firm A's run id":      s.runA,
		"firm A's artifact id": s.artA,
		"firm A's citation id": s.citA,
		"firm A's tenant id":   s.a.ID,
	} {
		// The run id legitimately appears in a 404 for that run's own path;
		// what must never appear is A's DATA.
		if label == "firm A's run id" {
			continue
		}
		if strings.Contains(body, needle) {
			t.Fatalf("the response leaked %s: %s", label, trimBody(body))
		}
	}
}

func trimBody(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// ===========================================================================
// 3. NO JUDGE ANALYTICS (§13)
// ===========================================================================

// COJ art. L. 111-13 criminally sanctions reusing MAGISTRATES' and CLERKS'
// identity data to evaluate, analyse, compare or predict their professional
// practices. §13 turns that into a product ban: no judge ranking, no
// judge-outcome prediction, no analytics whose object or effect is the
// comparison of named magistrates, and nothing intended to reverse
// pseudonymisation or enrich party identities.
//
// A ban is only real if something checks it, and the realistic failure is not
// a malicious commit — it is a plausible-sounding feature request ("show which
// chamber rules our way most often") landing on a Friday. So this is a grep,
// it runs in CI, and it covers the Go API, the SvelteKit front and the locale
// files where such a feature would announce itself in French.
//
// The patterns are written to catch the FEATURE, not the vocabulary. French
// legal text is full of "juge", "jugement" and "judiciaire"; what is banned is
// those words next to ranking, scoring, prediction, profiling or comparison.
// judgeNoun matches a MAGISTRATE, not the verb, and it is deliberately NOT
// case-insensitive: `(?i)` would turn `[_A-Z]` into `[_A-Za-z]` and make
// "gets judged" look like an identifier. "jugement", "judiciaire", "Judilibre"
// and "gets judged" are ordinary legal prose and must never trip the guard;
// "juges", "JudgeScore", "juge_ranking", "MagistrateStats" and "MAGISTRAT_ID"
// are the thing COJ art. L. 111-13 is about.
const judgeNoun = `([Jj]udges?\b|JUDGE|[Jj]udge[_A-Z]|[Jj]uges?\b|JUGE|[Jj]uge[_A-Z]|` +
	`[Mm]agistrate?s?\b|MAGISTRAT|[Mm]agistrate?[_A-Z]|[Rr]apporteurs?\b|[Gg]reffiers?\b)`

// analyticsVerb is what may not be done TO a magistrate: evaluate, analyse,
// compare or predict their professional practices.
const analyticsVerb = `([Rr]ank|RANK|[Ss]cor|SCOR|[Pp]rofil|PROFIL|[Ss]tatistic|[Aa]nalytic|` +
	`[Bb]enchmark|[Ll]eaderboard|[Ww]in[_Rr]ate|WIN_RATE|[Pp]r[ée]dic|[Pp]redict|PREDICT|[Nn]otation)`

// prohibitionProse recognises a line that SAYS the feature is banned. Code
// cannot state a prohibition, so this can only ever excuse prose.
var prohibitionProse = regexp.MustCompile(`(?i)(\bno\b|\bnot\b|\bnever\b|\bjamais\b|interdi|forbid|prohibit|\bbans?\b|\bbanned\b|l\.\s?111-13)`)

var bannedJudgeAnalytics = []struct {
	what string
	re   *regexp.Regexp
}{
	{"judge ranking / scoring / profiling",
		regexp.MustCompile(analyticsVerb + `[^\n]{0,24}` + judgeNoun)},
	{"judge ranking / scoring / profiling (reversed)",
		regexp.MustCompile(judgeNoun + `[^\n]{0,24}` + analyticsVerb)},
	{"outcome prediction",
		regexp.MustCompile(`([Pp]redict|PREDICT|[Pp]r[ée]dic|[Pp]ronostic|[Pp]robabilit[ée] de (succ[èe]s|gain))` +
			`[^\n]{0,24}([Oo]utcome|OUTCOME|issue du litige|[Vv]erdict|` + judgeNoun + `)`)},
	{"named-magistrate comparison",
		regexp.MustCompile(`([Cc]ompar|COMPAR)[^\n]{0,24}(` + judgeNoun + `|[Ff]ormation de jugement)`)},
	{"re-identification / de-pseudonymisation",
		regexp.MustCompile(`(?i)\b(re[_-]?identif|r[ée][_-]?identif|d[ée]sanonymis|de[_-]?anonymis|de[_-]?pseudonymis|d[ée]pseudonymis|unmask)`)},
}

// A guard that cannot fail is decoration. These are the feature requests the
// grep above exists to reject; every one of them must match something.
func TestNoJudgeAnalyticsGuardActuallyCatches(t *testing.T) {
	shouldMatch := []string{
		`func RankJudges(ctx context.Context) ([]JudgeScore, error) {`,
		`type MagistrateStats struct { WinRate float64 }`,
		`"juge_scoring": "Notation des juges",`,
		`predictOutcome(judge, facts)`,
		`// compare les juges de la formation`,
		`func reIdentifyParty(decision string) string {`,
		`const desanonymisation = true`,
	}
	for _, s := range shouldMatch {
		matched := false
		for _, b := range bannedJudgeAnalytics {
			if b.re.MatchString(s) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("the judge-analytics guard would MISS this: %s", s)
		}
	}

	// The prose allowance must excuse a line that STATES the ban…
	for _, s := range []string{
		"// (§13): no judge ranking, judge-outcome prediction or any analytics whose",
		"// Nomos ne propose jamais de notation des magistrats.",
	} {
		if !prohibitionProse.MatchString(s) {
			t.Errorf("a line documenting the ban is not recognised as prose: %s", s)
		}
	}
	// …and must NOT excuse code that does it.
	for _, s := range []string{
		`func RankJudges(ctx context.Context) ([]JudgeScore, error) {`,
		`type MagistrateStats struct { WinRate float64 }`,
		`predictOutcome(judge, facts)`,
	} {
		if prohibitionProse.MatchString(s) {
			t.Errorf("the prose allowance would excuse actual code: %s", s)
		}
	}

	// And it must not fire on the ordinary French legal vocabulary the corpus
	// is full of, or the guard gets disabled the first week.
	shouldNotMatch := []string{
		`// jugement rendu par la chambre sociale`,
		"Jurisdiction string `json:\"jurisdiction\"`",
		`"judilibre": "Judilibre (Cour de cassation)",`,
		`// the run row therefore identify the workflow definition`,
		`PublicationLevel string // publié au Bulletin`,
		`score := rrf(vectorRank, ftsRank)`,
		`// rerank the top 40 candidates by topical relevance`,
	}
	for _, s := range shouldNotMatch {
		for _, b := range bannedJudgeAnalytics {
			if b.re.MatchString(s) {
				t.Errorf("the judge-analytics guard FALSE-POSITIVES on ordinary legal vocabulary (%s): %s", b.what, s)
			}
		}
	}
}

func TestNoJudgeAnalyticsAnywhere(t *testing.T) {
	root := repoRoot(t)

	banned := bannedJudgeAnalytics

	// Files that legitimately DISCUSS the ban: the plan, this suite, and any
	// code comment that names the prohibition. Prose is allowed to say what
	// is forbidden; code is not allowed to do it. The vendored WebGL effect
	// libraries are skipped too: they render light on a surface, and their
	// "unmasked" is a graphics mask, not a pseudonymised party.
	skip := regexp.MustCompile(`(?i)(docs/PLAN\.md|safety_test\.go|/node_modules/|/\.git/|/build/|/\.svelte-kit/|package-lock\.json|go\.sum|front/src/lib/components/libraries/)`)

	type finding struct {
		path string
		n    int
		line string
		what string
	}
	var findings []finding

	for _, sub := range []string{"api", "front/src", "front/locales", "docs"} {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		for _, b := range banned {
			hits := grepTree(t, dir, b.re, func(path string) bool {
				if skip.MatchString(filepath.ToSlash(path)) {
					return false
				}
				switch filepath.Ext(path) {
				case ".go", ".ts", ".js", ".svelte", ".json", ".md", ".sql", ".conf":
					return true
				}
				return false
			})
			for _, h := range hits {
				// A line that STATES the prohibition documents the ban; it does
				// not break it. The allowance is a prose test, deliberately
				// narrow: it needs an explicit negation or the article
				// reference on the same line, so an identifier like
				// `JudgeScore` can never slip through it.
				if prohibitionProse.MatchString(h.line) {
					continue
				}

				findings = append(findings, finding{h.path, h.n, strings.TrimSpace(h.line), b.what})
			}
		}
	}

	for _, f := range findings {
		t.Errorf("BANNED FEATURE (%s) — %s:%d\n\t%s\n\t(COJ art. L. 111-13 / docs/PLAN.md §13)",
			f.what, f.path, f.n, f.line)
	}
	if len(findings) == 0 {
		t.Log("no judge-ranking, judge-prediction, magistrate-comparison or re-identification code anywhere")
	}
}

// The data model must not even be ABLE to hold what the ban forbids: there is
// no column anywhere for a magistrate's identity, so no analytics can be built
// on one without a schema change somebody has to justify.
func TestNoMagistrateIdentityColumn(t *testing.T) {
	root := repoRoot(t)
	models := filepath.Join(root, "api", "types", "nomos_models.go")
	body, err := os.ReadFile(models)
	if err != nil {
		t.Fatalf("reading the model file: %v", err)
	}

	// Formation ("Soc.", "Chambre sociale") is a COURT formation, not a
	// person, and §6.3 needs it to judge authority. A named individual is the
	// line.
	forbidden := regexp.MustCompile(`(?i)^\s*(JudgeName|JudgeID|Magistrate\w*|Rapporteur\w*|Greffier\w*|PresidentName|ConseillerName)\s`)
	for i, line := range strings.Split(string(body), "\n") {
		if forbidden.MatchString(line) {
			t.Errorf("%s:%d declares a magistrate-identity field:\n\t%s", models, i+1, strings.TrimSpace(line))
		}
	}

	// And the same must be true of what is actually in Postgres.
	testsupport.Boot(t)
	var cols []struct {
		TableName  string
		ColumnName string
	}
	if err := u.DB.Raw(`
		SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND (column_name ~* '(judge|magistrat|greffier|rapporteur)')`).Scan(&cols).Error; err != nil {
		t.Fatalf("column scan: %v", err)
	}
	for _, c := range cols {
		t.Errorf("the database holds %s.%s — a magistrate-identity column", c.TableName, c.ColumnName)
	}
	t.Logf("no magistrate-identity field in the models, %d matching columns in Postgres", len(cols))
}

// ===========================================================================
// 4. THE CASBIN DOMAIN MUST BE THE TENANT (§6.1)
// ===========================================================================

// "The starter's Casbin domain is `*`. Using that here would erase tenancy.
// The domain must be tenant_id from day one." (§6.1)
//
// This is a wiring property, not a runtime one — the route table is built
// inline in main(), so it cannot be exercised by an HTTP test. It is checked
// by reading the source, which is the only place the mistake can be made.
func TestPrivateRoutesAuthorizeInTheTenantDomain(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "api", "main.go"))
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	bad := regexp.MustCompile(`RequirePermission\("dossier"`)
	for i, line := range strings.Split(string(body), "\n") {
		if bad.MatchString(line) {
			t.Errorf("api/main.go:%d authorizes a private route in the global domain:\n\t%s",
				i+1, strings.TrimSpace(line))
		}
	}
}

// What CAN be asserted at runtime, and is: the tenant repository and the
// authorization gate can never disagree about which firm a request belongs to,
// because they read the same context key — and a firm's policies are never
// seeded into the global domain.
func TestTenantPoliciesAreNeverSeededInTheGlobalDomain(t *testing.T) {
	testsupport.Boot(t)
	firm := testsupport.NewFirm(t, "domainguard")

	if err := u.SeedTenantPolicies(u.DefaultDomain); err == nil {
		t.Fatal("SeedTenantPolicies accepted the global domain — a permission granted in \"*\" is a permission in every tenant at once")
	}

	// The firm's user is authorized IN ITS OWN DOMAIN…
	ok, err := u.Enforce(firm.UserID, firm.ID, types.ObjectDossier, types.ActionRead)
	if err != nil {
		t.Fatalf("Enforce in the firm domain: %v", err)
	}
	if !ok {
		t.Fatal("the firm's own user is not authorized in the firm's domain")
	}
	// …and nowhere else.
	other := testsupport.NewFirm(t, "domainother")
	ok, err = u.Enforce(firm.UserID, other.ID, types.ObjectDossier, types.ActionRead)
	if err != nil {
		t.Fatalf("Enforce in a foreign domain: %v", err)
	}
	if ok {
		t.Fatal("a user is authorized inside another firm's Casbin domain")
	}
	if n := testsupport.Count(t, "casbin_rule", "v1 = ?", u.DefaultDomain); n > 0 {
		var rules []struct{ V0, V1, V2, V3 string }
		u.DB.Raw(`SELECT v0, v1, v2, v3 FROM casbin_rule WHERE v1 = ?`, u.DefaultDomain).Scan(&rules)
		for _, r := range rules {
			if r.V2 == types.ObjectDossier {
				t.Fatalf("a dossier policy exists in the global domain: %v", r)
			}
		}
	}
	t.Log("authorization is scoped to the firm domain in both directions")
}

// ===========================================================================
// Source-tree helpers
// ===========================================================================

type grepHit struct {
	path string
	n    int
	line string
}

// repoRoot walks up from the test's working directory (api/) to the module
// root, so a guard test can grep the front end too.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docs", "PLAN.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find the repository root from %s", dir)
	return ""
}

// grepTree is a small, dependency-free recursive grep. A guard test that
// shells out to `grep` is a guard test that does not run on somebody's laptop.
func grepTree(t *testing.T, root string, re *regexp.Regexp, want func(path string) bool) []grepHit {
	t.Helper()
	var hits []grepHit
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", ".svelte-kit", "build", "dist", "tmp":
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() > 2<<20 {
			return nil
		}
		if want != nil && !want(path) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if re.MatchString(line) {
				hits = append(hits, grepHit{path: rel(root, path), n: i + 1, line: line})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return hits
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(filepath.Join(filepath.Base(root), r))
	}
	return path
}

var _ = fmt.Sprintf // keep fmt available for ad-hoc debugging of a failing guard
