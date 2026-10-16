// Package testsupport is the shared fixture layer for Nomos' durability and
// safety suites (api/durability_test.go, api/safety_test.go).
//
// It exists because those suites test PROPERTIES, not functions:
// at-least-once execution, lease recovery, transaction boundaries, idempotent
// publication, tenant isolation and citation refusal. Every one of them is
// only real against a live Postgres — FOR UPDATE SKIP LOCKED, the conditional
// lease transition and the `WHERE tenant_id = ?` predicate cannot be proven
// against a fake. So this package's whole job is to make "a real database,
// two real firms, a real router" cheap enough that the properties get tested
// instead of asserted in a comment.
//
// Nothing here is used by production code. It is a test-only package that
// happens to live in a non-_test file so several test packages can share it.
package testsupport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/controllers"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// DefaultDSN is the dev-stack Postgres (pgvector + unaccent), used when
// DATABASE_URL is not already set — never under NOMOS_TEST_NO_DOTENV=1.
const DefaultDSN = "postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"

// TenantPrefix marks every fixture tenant so Cleanup can find its rows and a
// human can find leftovers with one LIKE.
const TenantPrefix = "safety-firm-"

var bootOnce sync.Once

// Boot loads api/.env (unless NOMOS_TEST_NO_DOTENV=1), initialises the shared
// clients once per process and skips the calling test when Postgres is
// unreachable. An unavailable database is a missing environment, not a broken
// invariant — except under NOMOS_REQUIRE_DEPENDENCIES=1, where it fails: a
// gate run must not report green on a suite that never ran (testmode).
func Boot(t *testing.T) {
	t.Helper()
	BootClients()
	if u.DB == nil || u.Enforcer == nil {
		testmode.Missing(t, "Postgres/Casbin not reachable — durability & safety suites skipped")
	}
}

// BootClients is Boot without a test: the environment is resolved and the
// shared clients initialised once per process. Callers check u.DB themselves.
func BootClients() {
	gin.SetMode(gin.TestMode)
	bootOnce.Do(func() {
		// The suites are run from api/ (package main) and from api/<pkg>/,
		// so both relative positions of the env file are tried.
		testmode.LoadDotenv(".env", "../.env")
		if dsn := testmode.DatabaseURL(DefaultDSN); dsn != "" {
			os.Setenv("DATABASE_URL", dsn)
		}
		if u.DB == nil {
			u.InitClients(config.Load())
		}
	})
}

// DB is the shared GORM handle, after Boot.
func DB(t *testing.T) *gorm.DB {
	t.Helper()
	Boot(t)
	return u.DB
}

// ---------------------------------------------------------------------------
// Firms
// ---------------------------------------------------------------------------

// Firm is one isolated tenant: a real firms row, a real Casbin domain, and one
// user holding the `user` role inside it. Two of these are what every
// cross-tenant attack in safety_test.go is played between.
type Firm struct {
	ID     string
	UserID string
}

// NewFirm provisions a tenant and registers its teardown. The id carries
// TenantPrefix so a crashed run leaves findable, deletable rows rather than
// anonymous ones.
func NewFirm(t *testing.T, label string) Firm {
	t.Helper()
	Boot(t)
	f, err := ProvisionFirm(label)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cleanup(f.ID, f.UserID) })
	return f
}

// ProvisionFirm is NewFirm without a test: the caller owns the teardown
// (Cleanup(f.ID, f.UserID)).
func ProvisionFirm(label string) (Firm, error) {
	f := Firm{
		ID:     TenantPrefix + label + "-" + uuid.NewString()[:8],
		UserID: "safety-user-" + label + "-" + uuid.NewString()[:8],
	}
	if err := u.EnsureFirm(context.Background(), f.ID, "Cabinet "+label, f.UserID); err != nil {
		return f, fmt.Errorf("EnsureFirm(%s): %w", f.ID, err)
	}
	if err := u.AddRoleForUserInDomain(f.UserID, types.RoleUser, f.ID); err != nil {
		return f, fmt.Errorf("AddRoleForUserInDomain(%s): %w", f.ID, err)
	}
	return f, nil
}

// Repo is a tenant-scoped repository for this firm, bound to a background
// context (t.Context() is already cancelled inside a Cleanup, which silently
// turns every scoped delete into a no-op).
func (f Firm) Repo() *u.TenantRepo { return u.ForTenantID(context.Background(), f.ID) }

// Cleanup removes every row a fixture tenant owns, in dependency order.
// Deliberately raw SQL: the point is to leave NOTHING behind, including rows a
// bug wrote under the wrong tenant.
func Cleanup(tenantID, userID string) {
	if u.DB == nil {
		return
	}
	db := u.DB
	runIDs := "(SELECT id FROM runs WHERE tenant_id = ?)"
	db.Exec(`DELETE FROM run_step_deps WHERE run_id IN `+runIDs, tenantID)
	for _, table := range []string{"run_events", "run_questions", "step_attempts"} {
		db.Exec(`DELETE FROM `+table+` WHERE tenant_id = ?`, tenantID)
	}
	for _, table := range []string{
		"agent_web_sources", "agent_verifications", "agent_document_revisions",
		"agent_evidence_spans", "agent_read_receipts", "agent_turns",
		"llm_calls", "citations", "artifacts", "run_steps", "runs",
		"piece_chunks", "piece_parties", "pieces", "assets",
		"messages", "conversations", "audit_events",
		"skill_versions", "skills", "memory_preferences", "dossiers",
	} {
		db.Exec(`DELETE FROM `+table+` WHERE tenant_id = ?`, tenantID)
	}
	db.Exec(`DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ?`, tenantID, tenantID)
	if userID != "" {
		db.Exec(`DELETE FROM casbin_rule WHERE v0 = ?`, userID)
	}
	db.Exec(`DELETE FROM firms WHERE id = ?`, tenantID)
	if u.Enforcer != nil {
		_ = u.Enforcer.LoadPolicy()
	}
}

// ---------------------------------------------------------------------------
// The router
// ---------------------------------------------------------------------------

// Router mounts the private Nomos surface with the REAL middleware chain,
// except that the Better Auth session lookup is replaced by a header-driven
// injection of the AuthUser RequireAuth would have produced. What is under
// test is everything that happens AFTER authentication.
//
// The chain is RequireTenant + RequireTenantPermission, i.e. Casbin enforced
// in the FIRM's domain. §6.1 is explicit that the starter's "*" domain would
// erase tenancy, so a suite that authorised in "*" would be testing a
// configuration the plan forbids.
func Router(t *testing.T) *gin.Engine {
	t.Helper()
	Boot(t)
	return NewRouter()
}

// NewRouter is Router without a test, for harnesses that boot themselves
// (api/eval/agent).
func NewRouter() *gin.Engine {
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
	api.POST("/dossiers/:id/pieces", write, controllers.IngestPieces)
	api.GET("/dossiers", read, controllers.ListDossiers)
	api.GET("/dossiers/:id", read, controllers.GetDossier)
	api.GET("/dossiers/:id/pieces", read, controllers.ListPieces)
	api.POST("/dossiers/:id/export", write, controllers.ExportDossier)
	api.PATCH("/pieces/:id", write, controllers.UpdatePiece)
	api.GET("/pieces/:id/download", read, controllers.DownloadPiece)

	api.GET("/runs/:id", read, controllers.GetRun)
	api.GET("/runs/:id/events", read, controllers.RunEvents)
	api.POST("/runs/:id/answers", write, controllers.AnswerRun)
	api.POST("/runs/:id/cancel", write, controllers.CancelRun)
	api.GET("/runs/:id/artifacts", read, controllers.RunArtifacts)
	api.GET("/artifacts/:id/download", read, controllers.DownloadArtifact)
	api.GET("/runs/:id/citations", read, controllers.RunCitations)
	api.GET("/citations/:id", read, controllers.GetCitation)
	api.GET("/runs/:id/coverage", read, controllers.RunCoverage)
	api.POST("/search", controllers.Search)

	api.POST("/conversations", controllers.CreateConversation)
	api.POST("/conversations/:id/messages", controllers.PostMessage)
	api.GET("/conversations/:id/events", controllers.ConversationEvents)

	api.GET("/skills", controllers.ListSkills)
	api.POST("/skills", controllers.CreateSkill)
	return r
}

// Do issues one request as userID.
func Do(r *gin.Engine, method, path, userID string, body any) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-Test-User", userID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Upload posts files as the multipart body the composer's drop zone sends:
// the repeated `files` field, one part per document, with the real filename.
// The extension is deliberately kept — ingestion sniffs the magic bytes and
// must be seen ignoring it.
func Upload(r *gin.Engine, path, userID string, paths []string) (*httptest.ResponseRecorder, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		part, err := w.CreateFormFile("files", filepath.Base(p))
		if err != nil {
			f.Close()
			return nil, err
		}
		if _, err := io.Copy(part, f); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Test-User", userID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec, nil
}

// Decode unmarshals a response body, failing the test with the raw body when
// it cannot — a decode error on an unexpected 500 is otherwise unreadable.
func Decode(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decoding response: %v — body was %s", err, string(b))
	}
}

// ---------------------------------------------------------------------------
// Assertions and waits
// ---------------------------------------------------------------------------

// WaitFor polls cond until it holds or the deadline passes. The runner is
// asynchronous by design; a sleep would be either flaky or slow.
func WaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Count is how a durable property is asserted: not "the function returned",
// but "the row is in Postgres".
func Count(t *testing.T, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := u.DB.Table(table).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// AssertDenied accepts 403 or 404 for a cross-tenant read. Both are correct:
// 404 hides existence, 403 refuses the act. 200 is a breach; 500 means the
// query ran and something else went wrong, which is not a defence.
func AssertDenied(t *testing.T, status int, what string) {
	t.Helper()
	switch status {
	case http.StatusForbidden, http.StatusNotFound:
		return
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		t.Fatalf("%s: CROSS-TENANT ACCESS SUCCEEDED with status %d", what, status)
	default:
		t.Fatalf("%s: expected 403/404, got %d", what, status)
	}
}

// ---------------------------------------------------------------------------
// Blob storage
// ---------------------------------------------------------------------------

// MemBlob is an in-memory BlobStore that COUNTS writes. Idempotent
// publication (§4.3) is a claim about how many objects exist after a replay,
// so the test needs a store that can be interrogated, not a bucket.
type MemBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    map[string]int
}

// NewMemBlob installs a fresh counting store as utils.Blob and restores the
// previous one when the test ends.
func NewMemBlob(t *testing.T) *MemBlob {
	t.Helper()
	m := &MemBlob{objects: map[string][]byte{}, puts: map[string]int{}}
	prev := u.Blob
	u.Blob = m
	t.Cleanup(func() { u.Blob = prev })
	return m
}

func (m *MemBlob) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	m.puts[key]++
	return nil
}

func (m *MemBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, u.ErrBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *MemBlob) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemBlob) SignedURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memblob://" + key, nil
}

func (m *MemBlob) Backend() string { return "mem" }

// Keys returns every key currently held, for an "exactly one object" assertion.
func (m *MemBlob) Keys(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objects {
		if prefix == "" || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	return out
}

// Puts reports how many times key was written — a replay that overwrites the
// same key is idempotent; one that writes a second key is not.
func (m *MemBlob) Puts(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts[key]
}

// ErrSkipNoBlob is returned when a fixture cannot stage an object.
var ErrSkipNoBlob = errors.New("blob store unavailable")
