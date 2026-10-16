package main

// Adversarial cross-tenant tests (docs/PLAN.md §6.1, §14).
//
// Tenant scoping in Nomos is application-level, not Postgres RLS: nothing in
// the database stops a SELECT from returning another firm's rows. The
// mitigation is utils.TenantRepo — and a mitigation nobody attacks is a
// mitigation nobody has tested. So this suite plays the attacker: two firms,
// a dossier and a piece each, and then firm B tries every read and write path
// against firm A's REAL uuids.
//
// "No cross-tenant object, event or run access under adversarial tests" is a
// slice-1 acceptance criterion (§14). If any assertion here starts failing,
// treat it as a confidentiality incident in the making, not a flaky test.
//
// Requires the local Postgres from the dev stack; skipped when DATABASE_URL is
// unreachable. Better Auth's `user`/`session` tables are NOT required: the
// suite injects the verified AuthUser the way RequireAuth would, because what
// is under test is everything that happens AFTER authentication.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/controllers"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/testsupport"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// tenantFixture is the whole attack setup: two firms that must never see each
// other, each with one user, one dossier and one piece.
type tenantFixture struct {
	router *gin.Engine

	firmA, firmB       string
	aliceID, bobID     string
	dossierA, dossierB string
	pieceA, pieceB     string
	titleA             string
}

func setupTenants(t *testing.T) *tenantFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	testmode.LoadDotenv(".env")
	if dsn := testmode.DatabaseURL(testsupport.DefaultDSN); dsn != "" {
		os.Setenv("DATABASE_URL", dsn)
	}
	// Keep the suite hermetic: no shared bucket, no gateway (OPENROUTER_API_KEY
	// falls back to OPENAI_API_KEY). Storage-dependent paths must still refuse
	// a cross-tenant id, which is exactly what the download test asserts.
	os.Unsetenv("S3_ACCESS_KEY")
	os.Unsetenv("S3_SECRET_KEY")
	os.Unsetenv("OPENAI_API_KEY")

	if u.DB == nil {
		u.InitClients(config.Load())
	}
	if u.DB == nil || u.Enforcer == nil {
		testmode.Missing(t, "Postgres/Casbin not reachable — cross-tenant suite skipped")
	}

	f := &tenantFixture{
		firmA:   "test_firm_a_" + uuid.NewString(),
		firmB:   "test_firm_b_" + uuid.NewString(),
		aliceID: "test_user_alice_" + uuid.NewString(),
		bobID:   "test_user_bob_" + uuid.NewString(),
		titleA:  "Prud'hommes — SECRET DE FIRME A " + uuid.NewString(),
	}

	ctx := t.Context()
	for _, firm := range []string{f.firmA, f.firmB} {
		if err := u.EnsureFirm(ctx, firm, "Cabinet de test", "test"); err != nil {
			t.Fatalf("EnsureFirm(%s): %v", firm, err)
		}
	}
	if err := u.AddRoleForUserInDomain(f.aliceID, types.RoleUser, f.firmA); err != nil {
		t.Fatalf("attach alice: %v", err)
	}
	if err := u.AddRoleForUserInDomain(f.bobID, types.RoleUser, f.firmB); err != nil {
		t.Fatalf("attach bob: %v", err)
	}

	t.Cleanup(func() { cleanupTenants(t, f) })

	f.router = testRouter()
	f.dossierA, f.pieceA = seedDossier(t, f, f.aliceID, f.titleA)
	f.dossierB, f.pieceB = seedDossier(t, f, f.bobID, "Litige — firme B")
	return f
}

// testRouter mounts the routes under test with the real middleware chain,
// except that the session lookup is replaced by a header-driven injection of
// the AuthUser RequireAuth would have produced.
func testRouter() *gin.Engine {
	r := gin.New()
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		id := c.GetHeader("X-Test-User")
		if id == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "Missing session token"})
			return
		}
		c.Set(middlewares.AuthUserKey, &u.AuthUser{ID: id, Email: id + "@example.test"})
		c.Next()
	})
	api.Use(middlewares.RequireTenant())

	read := middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead)
	write := middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite)

	api.POST("/dossiers", write, controllers.CreateDossier)
	api.GET("/dossiers", read, controllers.ListDossiers)
	api.GET("/dossiers/:id", read, controllers.GetDossier)
	api.DELETE("/dossiers/:id", write, controllers.DeleteDossier)
	api.GET("/dossiers/:id/pieces", read, controllers.ListPieces)
	api.PATCH("/pieces/:id", write, controllers.UpdatePiece)
	api.GET("/pieces/:id/download", read, controllers.DownloadPiece)
	return r
}

// seedDossier creates a dossier through the real API (so the tenant is derived
// exactly as it is in production) and a piece through the tenant repository.
func seedDossier(t *testing.T, f *tenantFixture, userID, title string) (dossierID, pieceID string) {
	t.Helper()

	w := do(f, http.MethodPost, "/api/dossiers", userID,
		map[string]any{"title": title, "client_reference": "REF-" + title})
	if w.Code != http.StatusCreated {
		t.Fatalf("create dossier for %s: status %d body %s", userID, w.Code, w.Body.String())
	}
	var d types.Dossier
	decode(t, w.Body.Bytes(), &d)

	repo := u.ForTenantID(t.Context(), d.TenantID)
	p := types.Piece{
		ID:                  uuid.NewString(),
		DossierID:           d.ID,
		SourceAssetID:       uuid.NewString(),
		OriginalName:        "contrat-" + d.ID + ".pdf",
		ProposedName:        "01-contrat.pdf",
		Ordinal:             1,
		PieceKind:           "contrat",
		Title:               "Contrat de travail",
		ClassificationState: types.PieceStateProposed,
	}
	asset := types.Asset{
		ID:     p.SourceAssetID,
		S3Key:  u.S3Prefix + d.TenantID + "/" + d.ID + "/" + p.ID + "-original.pdf",
		Kind:   types.AssetKindOriginal,
		Mime:   "application/pdf",
		SHA256: strings.Repeat("a", 64),
	}
	if err := repo.Create(&asset); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if err := repo.Create(&p); err != nil {
		t.Fatalf("seed piece: %v", err)
	}
	return d.ID, p.ID
}

func cleanupTenants(t *testing.T, f *tenantFixture) {
	t.Helper()
	// NOT t.Context(): it is already cancelled by the time a Cleanup runs, and
	// every scoped query is context-bound — the deletes would silently no-op
	// and leave the fixtures behind.
	ctx := context.Background()
	for _, firm := range []string{f.firmA, f.firmB} {
		repo := u.ForTenantID(ctx, firm)
		repo.Pieces().Where("tenant_id = ?", firm).Delete(&types.Piece{})
		repo.Assets().Where("tenant_id = ?", firm).Delete(&types.Asset{})
		repo.AuditEvents().Where("tenant_id = ?", firm).Delete(&types.AuditEvent{})
		repo.Dossiers().Where("tenant_id = ?", firm).Delete(&types.Dossier{})
		u.DB.Exec("DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ?", firm, firm)
		u.DB.Exec("DELETE FROM firms WHERE id = ?", firm)
	}
	u.DB.Exec("DELETE FROM casbin_rule WHERE v0 IN (?, ?)", f.aliceID, f.bobID)
	if err := u.Enforcer.LoadPolicy(); err != nil {
		t.Logf("reloading casbin policy after cleanup: %v", err)
	}
}

// --- the attacks ------------------------------------------------------------

// TestCrossTenantDossierAccess is the core assertion: firm B, holding firm A's
// real uuids, gets nothing on any read or write path.
func TestCrossTenantDossierAccess(t *testing.T) {
	f := setupTenants(t)

	t.Run("get another tenant's dossier by raw uuid", func(t *testing.T) {
		w := do(f, http.MethodGet, "/api/dossiers/"+f.dossierA, f.bobID, nil)
		assertDenied(t, w.Code)
		assertNoLeak(t, w.Body.String(), f)
	})

	t.Run("list never crosses the tenant", func(t *testing.T) {
		w := do(f, http.MethodGet, "/api/dossiers?limit=200", f.bobID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list: status %d body %s", w.Code, w.Body.String())
		}
		assertNoLeak(t, w.Body.String(), f)

		var page struct {
			Items []types.Dossier `json:"items"`
			Total int64           `json:"total"`
		}
		decode(t, w.Body.Bytes(), &page)
		if page.Total != int64(len(page.Items)) || page.Total != 1 {
			t.Fatalf("bob should see exactly his one dossier, got total=%d items=%d", page.Total, len(page.Items))
		}
		for _, d := range page.Items {
			if d.TenantID != f.firmB {
				t.Fatalf("list leaked a row from tenant %s", d.TenantID)
			}
		}
	})

	t.Run("piece list of another tenant's dossier", func(t *testing.T) {
		w := do(f, http.MethodGet, "/api/dossiers/"+f.dossierA+"/pieces", f.bobID, nil)
		assertDenied(t, w.Code)
		assertNoLeak(t, w.Body.String(), f)
	})

	t.Run("patch another tenant's piece", func(t *testing.T) {
		w := do(f, http.MethodPatch, "/api/pieces/"+f.pieceA, f.bobID,
			map[string]any{"title": "pwned", "ordinal": 99})
		assertDenied(t, w.Code)
		assertNoLeak(t, w.Body.String(), f)

		var after types.Piece
		if err := u.ForTenantID(t.Context(), f.firmA).Pieces().
			Where("id = ?", f.pieceA).First(&after).Error; err != nil {
			t.Fatalf("reloading firm A's piece: %v", err)
		}
		if after.Title == "pwned" || after.Ordinal == 99 {
			t.Fatal("CROSS-TENANT WRITE: firm B modified firm A's piece")
		}
	})

	t.Run("download another tenant's piece", func(t *testing.T) {
		w := do(f, http.MethodGet, "/api/pieces/"+f.pieceA+"/download", f.bobID, nil)
		assertDenied(t, w.Code)
		assertNoLeak(t, w.Body.String(), f)

		// The owner must NOT get the same answer, or the 404 above would be
		// storage misconfiguration rather than tenant isolation.
		own := do(f, http.MethodGet, "/api/pieces/"+f.pieceA+"/download", f.aliceID, nil)
		if own.Code == http.StatusNotFound {
			t.Fatalf("owner got 404 too — the cross-tenant 404 proves nothing; body %s", own.Body.String())
		}
	})

	t.Run("delete another tenant's dossier", func(t *testing.T) {
		w := do(f, http.MethodDelete, "/api/dossiers/"+f.dossierA, f.bobID, nil)
		assertDenied(t, w.Code)

		var n int64
		if err := u.ForTenantID(t.Context(), f.firmA).Dossiers().
			Where("id = ?", f.dossierA).Count(&n).Error; err != nil {
			t.Fatalf("recount firm A's dossier: %v", err)
		}
		if n != 1 {
			t.Fatal("CROSS-TENANT DELETE: firm B purged firm A's dossier")
		}
	})

	t.Run("the owner still can", func(t *testing.T) {
		w := do(f, http.MethodGet, "/api/dossiers/"+f.dossierA, f.aliceID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("owner denied their own dossier: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), f.titleA) {
			t.Fatal("owner's own dossier came back without its title")
		}
	})
}

// TestTenantRepositoryScoping attacks the repository directly — below the HTTP
// layer, where a forgotten predicate would actually happen. Run events and run
// rows are included because "no cross-tenant object, event or run access" is
// the acceptance criterion, and those tables are read by another agent's
// services through this same repository.
func TestTenantRepositoryScoping(t *testing.T) {
	f := setupTenants(t)
	ctx := t.Context()
	repoA := u.ForTenantID(ctx, f.firmA)
	repoB := u.ForTenantID(ctx, f.firmB)

	// Firm A owns a run, an event, a chunk and a party for its dossier.
	runID := uuid.NewString()
	if err := repoA.Create(&types.Run{
		ID: runID, DossierID: f.dossierA, RequestedBy: f.aliceID,
		WorkflowType: types.WorkflowDossierOrganizeV1, State: types.RunStatePending,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := repoA.Create(&types.RunEvent{RunID: &runID, Type: types.EventRunCreated}); err != nil {
		t.Fatalf("seed run event: %v", err)
	}
	if err := repoA.Create(&types.PieceChunk{
		DossierID: f.dossierA, PieceID: f.pieceA, Ordinal: 0, Text: "clause de non-concurrence",
	}); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	if err := repoA.Create(&types.PiecePartie{
		ID: uuid.NewString(), PieceID: f.pieceA, Role: types.PartyRoleParty,
		DisplayName: "Société A", NormalizedName: "societe a",
	}); err != nil {
		t.Fatalf("seed party: %v", err)
	}
	t.Cleanup(func() {
		repoA := u.ForTenantID(context.Background(), f.firmA)
		repoA.RunEvents().Where("run_id = ?", runID).Delete(&types.RunEvent{})
		repoA.Runs().Where("id = ?", runID).Delete(&types.Run{})
		repoA.PieceChunks().Where("piece_id = ?", f.pieceA).Delete(&types.PieceChunk{})
		repoA.PieceParties().Where("piece_id = ?", f.pieceA).Delete(&types.PiecePartie{})
	})

	// Firm B addresses every one of them by its real id.
	for _, probe := range []struct {
		name string
		q    func() (int64, error)
	}{
		{"dossiers", func() (int64, error) { return count(repoB.Dossiers().Where("id = ?", f.dossierA)) }},
		{"pieces", func() (int64, error) { return count(repoB.Pieces().Where("id = ?", f.pieceA)) }},
		{"assets", func() (int64, error) { return count(repoB.Assets().Where("id IS NOT NULL")) }},
		{"piece_chunks", func() (int64, error) { return count(repoB.PieceChunks().Where("piece_id = ?", f.pieceA)) }},
		{"piece_parties", func() (int64, error) { return count(repoB.PieceParties().Where("piece_id = ?", f.pieceA)) }},
		{"runs", func() (int64, error) { return count(repoB.Runs().Where("id = ?", runID)) }},
		{"run_events", func() (int64, error) { return count(repoB.RunEvents().Where("run_id = ?", runID)) }},
		{"audit_events", func() (int64, error) { return count(repoB.AuditEvents().Where("object_id = ?", f.dossierA)) }},
	} {
		n, err := probe.q()
		if err != nil {
			t.Fatalf("%s probe failed: %v", probe.name, err)
		}
		if probe.name == "assets" {
			// Firm B sees its own asset and only its own.
			if n != 1 {
				t.Fatalf("assets: firm B sees %d rows, expected exactly its own 1", n)
			}
			continue
		}
		if n != 0 {
			t.Fatalf("CROSS-TENANT READ: firm B's repository returned %d %s row(s) belonging to firm A", n, probe.name)
		}
	}

	// And a write cannot escape either: Create stamps the repository's tenant
	// over whatever the caller put in the struct.
	smuggled := types.Dossier{
		ID: uuid.NewString(), TenantID: f.firmA, Title: "smuggled", State: types.DossierStateActive,
	}
	if err := repoB.Create(&smuggled); err != nil {
		t.Fatalf("smuggled create: %v", err)
	}
	t.Cleanup(func() {
		u.ForTenantID(context.Background(), f.firmB).Dossiers().
			Where("id = ?", smuggled.ID).Delete(&types.Dossier{})
	})
	if smuggled.TenantID != f.firmB {
		t.Fatalf("Create honoured a caller-supplied tenant_id (%s) — it must stamp %s", smuggled.TenantID, f.firmB)
	}
	n, err := count(repoA.Dossiers().Where("id = ?", smuggled.ID))
	if err != nil {
		t.Fatalf("smuggled probe: %v", err)
	}
	if n != 0 {
		t.Fatal("CROSS-TENANT WRITE: a row addressed to firm A was accepted through firm B's repository")
	}
}

// TestTenantCasbinDomainIsFirmID pins the decision docs/PLAN.md §6.1 calls out
// by name: the Casbin domain is the firm id. The starter's DefaultDomain "*"
// would grant a permission in every tenant at once, which is not a weaker
// tenancy — it is no tenancy.
func TestTenantCasbinDomainIsFirmID(t *testing.T) {
	f := setupTenants(t)

	if u.DefaultDomain != "*" {
		t.Fatalf("assumption changed: utils.DefaultDomain is %q", u.DefaultDomain)
	}
	if err := u.SeedTenantPolicies(u.DefaultDomain); err == nil {
		t.Fatal("SeedTenantPolicies accepted the global domain — tenancy would be erased")
	}

	cases := []struct {
		user, domain string
		want         bool
		why          string
	}{
		{f.aliceID, f.firmA, true, "alice may read dossiers in her own firm"},
		{f.bobID, f.firmB, true, "bob may read dossiers in his own firm"},
		{f.aliceID, f.firmB, false, "alice has no role in firm B's domain"},
		{f.bobID, f.firmA, false, "bob has no role in firm A's domain"},
		{f.aliceID, u.DefaultDomain, false, "no Nomos permission is granted in the global domain"},
		{f.bobID, u.DefaultDomain, false, "no Nomos permission is granted in the global domain"},
	}
	for _, tc := range cases {
		got, err := u.Enforce(tc.user, tc.domain, types.ObjectDossier, types.ActionRead)
		if err != nil {
			t.Fatalf("enforce(%s, %s): %v", tc.user, tc.domain, err)
		}
		if got != tc.want {
			t.Fatalf("enforce(dom=%s) = %v, want %v — %s", tc.domain, got, tc.want, tc.why)
		}
	}

	// The seeded policy rows themselves must carry the firm id, not "*".
	var rows []struct{ V0, V1, V2, V3 string }
	if err := u.DB.Raw(
		"SELECT v0, v1, v2, v3 FROM casbin_rule WHERE ptype = 'p' AND v1 = ? AND v2 = ?",
		f.firmA, types.ObjectDossier).Scan(&rows).Error; err != nil {
		t.Fatalf("reading casbin_rule: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no dossier policy was seeded in the firm's domain")
	}
	for _, r := range rows {
		if r.V1 != f.firmA {
			t.Fatalf("policy %v is not scoped to the firm id", r)
		}
	}
}

// TestTenantResolutionRejectsAnonymous makes sure the repository cannot be
// obtained without an identity: no session, no tenant, no query.
func TestTenantResolutionRejectsAnonymous(t *testing.T) {
	f := setupTenants(t)

	req := httptest.NewRequest(http.MethodGet, "/api/dossiers", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: status %d, want 401 (body %s)", w.Code, w.Body.String())
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/dossiers", nil)
	if _, err := u.ForTenant(c); err == nil {
		t.Fatal("utils.ForTenant returned a repository for a request with no user")
	}
}

// --- helpers ----------------------------------------------------------------

func do(f *tenantFixture, method, path, userID string, body any) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", userID)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decoding response: %v (body %s)", err, string(b))
	}
}

// count runs a tenant-scoped builder and returns how many rows it can see.
func count(q *gorm.DB) (int64, error) {
	var n int64
	err := q.Count(&n).Error
	return n, err
}

// assertDenied requires 404 or 403 — never a 2xx, and never a 5xx that would
// hint the row exists.
func assertDenied(t *testing.T, status int) {
	t.Helper()
	if status != http.StatusNotFound && status != http.StatusForbidden {
		t.Fatalf("cross-tenant request returned %d, want 404 or 403", status)
	}
}

// assertNoLeak fails if any identifier or title belonging to firm A appears in
// a response served to firm B — a denied status with the row in the body would
// still be a breach.
func assertNoLeak(t *testing.T, body string, f *tenantFixture) {
	t.Helper()
	for _, secret := range []string{f.titleA, f.dossierA, f.pieceA, f.firmA} {
		if strings.Contains(body, secret) {
			t.Fatalf("response to firm B leaked firm A's %s", trunc(secret))
		}
	}
}

func trunc(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

// TestTenantDossierPurge covers the other half of §9.1(8): DELETE is a real
// purge, not a soft delete, and it is honest when it cannot finish.
func TestTenantDossierPurge(t *testing.T) {
	f := setupTenants(t)
	ctx := t.Context()
	repoA := u.ForTenantID(ctx, f.firmA)

	t.Run("purges rows when there is nothing left in object storage", func(t *testing.T) {
		w := do(f, http.MethodPost, "/api/dossiers", f.aliceID, map[string]any{"title": "à purger"})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var d types.Dossier
		decode(t, w.Body.Bytes(), &d)

		// A piece whose original is already gone from S3 (no asset row).
		piece := types.Piece{
			ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: uuid.NewString(),
			OriginalName: "note.pdf", ClassificationState: types.PieceStateProposed,
		}
		if err := repoA.Create(&piece); err != nil {
			t.Fatalf("seed piece: %v", err)
		}
		if err := repoA.Create(&types.PieceChunk{
			DossierID: d.ID, PieceID: piece.ID, Ordinal: 0, Text: "contenu privé",
		}); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}

		w = do(f, http.MethodDelete, "/api/dossiers/"+d.ID, f.aliceID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("purge: %d %s", w.Code, w.Body.String())
		}
		var res struct {
			Complete bool `json:"complete"`
			Pieces   int  `json:"pieces"`
		}
		decode(t, w.Body.Bytes(), &res)
		if !res.Complete || res.Pieces != 1 {
			t.Fatalf("purge reported complete=%v pieces=%d", res.Complete, res.Pieces)
		}

		for name, q := range map[string]*gorm.DB{
			"dossiers":     repoA.Dossiers().Where("id = ?", d.ID),
			"pieces":       repoA.Pieces().Where("dossier_id = ?", d.ID),
			"piece_chunks": repoA.PieceChunks().Where("dossier_id = ?", d.ID),
		} {
			n, err := count(q)
			if err != nil {
				t.Fatalf("%s recount: %v", name, err)
			}
			if n != 0 {
				t.Fatalf("purge left %d %s row(s) behind — this is a soft delete, not a purge", n, name)
			}
		}
	})

	t.Run("purges the originals through the blob store the uploads used", func(t *testing.T) {
		// In development the originals live in the local blob store, not S3.
		// The purge used to talk to S3 directly, so without it a dossier
		// could never finish purging and sat in purge_pending forever.
		w := do(f, http.MethodPost, "/api/dossiers", f.aliceID, map[string]any{"title": "originaux locaux"})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var d types.Dossier
		decode(t, w.Body.Bytes(), &d)

		key := u.DossierKey(f.firmA, d.ID, "original.pdf")
		if err := u.Blob.Put(ctx, key, strings.NewReader("%PDF-1.4"), 8, "application/pdf"); err != nil {
			t.Fatalf("store original: %v", err)
		}
		asset := types.Asset{
			ID: uuid.NewString(), S3Key: key, Kind: types.AssetKindOriginal,
			Mime: "application/pdf", SHA256: strings.Repeat("b", 64),
		}
		if err := repoA.Create(&asset); err != nil {
			t.Fatalf("seed asset: %v", err)
		}
		if err := repoA.Create(&types.Piece{
			ID: uuid.NewString(), DossierID: d.ID, SourceAssetID: asset.ID,
			OriginalName: "original.pdf", ClassificationState: types.PieceStateProposed,
		}); err != nil {
			t.Fatalf("seed piece: %v", err)
		}

		w = do(f, http.MethodDelete, "/api/dossiers/"+d.ID, f.aliceID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("purge: %d %s", w.Code, w.Body.String())
		}
		var res struct {
			Complete       bool `json:"complete"`
			ObjectsRemoved int  `json:"objects_removed"`
		}
		decode(t, w.Body.Bytes(), &res)
		if !res.Complete || res.ObjectsRemoved != 1 {
			t.Fatalf("purge reported complete=%v objects_removed=%d, want a complete purge of 1 object",
				res.Complete, res.ObjectsRemoved)
		}
		if _, err := u.Blob.Get(ctx, key); !errors.Is(err, u.ErrBlobNotFound) {
			t.Fatalf("the original is still in the blob store (err=%v)", err)
		}
		if n, err := count(repoA.Dossiers().Where("id = ?", d.ID)); err != nil || n != 0 {
			t.Fatalf("dossier row survived a complete purge (n=%d err=%v)", n, err)
		}
	})

	t.Run("refuses to finish while objects remain", func(t *testing.T) {
		// Storage is unconfigured in this suite, so the originals behind the
		// fixture dossier cannot be removed. The purge must say so instead of
		// deleting the rows that point at them.
		w := do(f, http.MethodDelete, "/api/dossiers/"+f.dossierA, f.aliceID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("purge: %d %s", w.Code, w.Body.String())
		}
		var res struct {
			Complete    bool `json:"complete"`
			ObjectsKept int  `json:"objects_kept"`
		}
		decode(t, w.Body.Bytes(), &res)
		if res.Complete || res.ObjectsKept != 1 {
			t.Fatalf("incomplete purge reported complete=%v objects_kept=%d", res.Complete, res.ObjectsKept)
		}

		var d types.Dossier
		if err := repoA.Dossiers().Where("id = ?", f.dossierA).First(&d).Error; err != nil {
			t.Fatalf("the dossier must survive an incomplete purge: %v", err)
		}
		if d.State != types.DossierStatePurgePending {
			t.Fatalf("state after an incomplete purge is %q, want %q", d.State, types.DossierStatePurgePending)
		}
		n, err := count(repoA.Assets().Where("id IS NOT NULL"))
		if err != nil {
			t.Fatalf("asset recount: %v", err)
		}
		if n != 1 {
			t.Fatalf("the asset row must be kept for the retry, found %d", n)
		}
	})
}
