package utils

// Tenant scoping — the mandatory repository layer (docs/PLAN.md §6.1, §9.1(12)).
//
// READ THIS BEFORE ADDING A QUERY.
//
// THIS IS NOT POSTGRES ROW-LEVEL SECURITY. There is no policy in the database
// that stops a SELECT from returning another firm's rows. Isolation here is
// application-level: a `WHERE tenant_id = ?` predicate that a human has to
// remember to write. True RLS would be better, but the "AutoMigrate only" rule
// (§2) gives us no credible way to create and version a policy lifecycle, so
// this is what we have — and it is ONE MISSED CLAUSE AWAY FROM A CROSS-TENANT
// BREACH. In a product that holds privileged legal dossiers, that breach is a
// violation of secret professionnel, not a bug report (§13).
//
// The mitigation is this file, and it only works if it is used exclusively:
//
//   - NO service may query a private table through a bare utils.DB. Ever.
//     Private = every table carrying a tenant_id column: dossiers, pieces,
//     piece_parties, piece_chunks, assets, conversations, messages, runs,
//     run_steps, step_attempts, run_events, run_questions, artifacts,
//     citations, llm_calls, audit_events, skills, skill_versions,
//     memory_preferences, and the agent_* tables (turns, read receipts,
//     evidence spans, document revisions, verifications, web sources).
//   - Every builder below applies the tenant predicate ITSELF, so a caller
//     cannot forget it. There is deliberately no escape hatch that hands back
//     an unscoped handle.
//   - Create() STAMPS tenant_id by reflection rather than trusting the caller,
//     because a row written under the wrong tenant is the same breach as a
//     read without the predicate.
//   - Every builder is already .WithContext(...)-bound, so a client disconnect
//     cancels the query instead of holding a connection.
//
// Casbin authorizes an ACTION; it does not constrain a GORM query. The two are
// complementary and neither substitutes for the other. And the Casbin domain
// used everywhere in Nomos is the FIRM ID — never the starter's DefaultDomain
// "*", which would erase tenancy entirely (§6.1).
//
// api/tenant_test.go is the adversarial cross-tenant suite that exists because
// of everything above. It must keep passing.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Arkel-ai/nomos/api/types"
)

var tenantLog = NewLogger("utils.tenant")

// TenantContextKey is the gin.Context key holding the resolved firm id.
// middlewares.RequireTenant sets it; RequirePermission-style gates and the
// repositories below both read the same value, so authorization and data
// access can never disagree about which tenant a request belongs to.
const TenantContextKey = "nomos_tenant"

// authUserContextKey mirrors middlewares.AuthUserKey. utils cannot import
// middlewares (middlewares imports utils), so the literal is duplicated here
// on purpose. If one changes, change both.
const authUserContextKey = "auth_user"

// DefaultFirmID is the seeded singleton tenant of the MVP. Tenancy is real
// from day one — the id is a real column value and a real Casbin domain — but
// slice 1 provisions exactly one firm, and every authenticated user maps to
// it until an explicit multi-firm provisioning flow exists.
const DefaultFirmID = "firm_default"

const defaultFirmName = "Cabinet"

// ErrNoTenant means the caller could not be attributed to a firm: no session
// user on the context, or a chain that skipped RequireAuth. It is never a
// reason to fall back to an unscoped query.
var ErrNoTenant = errors.New("no tenant on request")

// ---------------------------------------------------------------------------
// Firms and tenant resolution
// ---------------------------------------------------------------------------

// EnsureFirm idempotently creates a firm row and seeds the role→permission
// matrix for its domain. Safe to call from several replicas at once: the
// insert is ON CONFLICT DO NOTHING and the policy seed is a no-op when the
// rule already exists.
func EnsureFirm(ctx context.Context, id, name, createdBy string) error {
	if DB == nil {
		return ErrAuthNotConfigured
	}
	if id == "" {
		return errors.New("firm id is required")
	}
	firm := types.Firm{ID: id, Name: name, CreatedBy: createdBy}
	if err := DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&firm).Error; err != nil {
		return err
	}
	if err := SeedTenantPolicies(id); err != nil && !errors.Is(err, ErrAuthNotConfigured) {
		return err
	}
	return nil
}

// SeedTenantPolicies gives the admin and user roles their permissions INSIDE
// the firm's own Casbin domain. The domain is the firm id, never "*": a
// permission granted in "*" is a permission in every tenant at once.
//
// Both roles get dossier read+write because a firm is a small, mutually
// trusting unit in slice 1; admin additionally administers the tenant. Refine
// per-dossier with the `dossier:<id>` object (§10) rather than by widening the
// domain.
func SeedTenantPolicies(domain string) error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	if domain == "" || domain == DefaultDomain {
		return fmt.Errorf("refusing to seed tenant policies in domain %q — the Casbin domain must be a firm id", domain)
	}
	for _, act := range []string{types.ActionRead, types.ActionWrite} {
		for _, role := range []string{types.RoleAdmin, types.RoleUser} {
			if err := EnsureRolePolicy(role, domain, types.ObjectDossier, act); err != nil {
				return err
			}
		}
		if err := EnsureRolePolicy(types.RoleAdmin, domain, types.ObjectAdmin, act); err != nil {
			return err
		}
	}
	return nil
}

// FirmExists reports whether a firm id names a real tenant. It is what stops a
// stray Casbin domain (the starter's "*", a stale grouping row) from being
// mistaken for a tenant.
func FirmExists(ctx context.Context, id string) (bool, error) {
	if DB == nil {
		return false, ErrAuthNotConfigured
	}
	var n int64
	if err := DB.WithContext(ctx).Model(&types.Firm{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// ResolveTenant maps an authenticated user id onto the firm id that becomes
// their tenant_id AND their Casbin domain.
//
// The user's Casbin domains are the source of truth, filtered to domains that
// are actually firms — so "*" and any stale grouping row can never be read as
// a tenant. A user with no firm is attached to the seeded singleton firm
// (§6.1: one firm in the MVP), which is also what makes the tenancy bootstrap
// lazy: the first authenticated request provisions it.
func ResolveTenant(ctx context.Context, userID string) (string, error) {
	if DB == nil {
		return "", ErrAuthNotConfigured
	}
	if userID == "" {
		return "", ErrNoTenant
	}

	if Enforcer != nil {
		domains, err := Enforcer.GetDomainsForUser(userID)
		if err != nil {
			return "", err
		}
		// Sorted so a user who somehow holds two firms resolves the same way
		// on every request rather than flapping between tenants.
		sort.Strings(domains)
		for _, d := range domains {
			if d == "" || d == DefaultDomain {
				continue
			}
			ok, err := FirmExists(ctx, d)
			if err != nil {
				return "", err
			}
			if ok {
				return d, nil
			}
		}
	}

	if err := EnsureFirm(ctx, DefaultFirmID, defaultFirmName, userID); err != nil {
		return "", err
	}
	if err := AddRoleForUserInDomain(userID, types.RoleUser, DefaultFirmID); err != nil {
		return "", err
	}
	tenantLog.Infof("Attached user to the seeded firm (firm=%s role=%s)", DefaultFirmID, types.RoleUser)
	return DefaultFirmID, nil
}

// TenantFromContext returns the firm id stashed by middlewares.RequireTenant,
// or "" when the chain has not resolved one yet.
func TenantFromContext(c *gin.Context) string {
	v, ok := c.Get(TenantContextKey)
	if !ok {
		return ""
	}
	id, _ := v.(string)
	return id
}

// ---------------------------------------------------------------------------
// The repository
// ---------------------------------------------------------------------------

// TenantRepo is a GORM handle that cannot address another tenant's rows. Get
// one with ForTenant(c); there is no constructor that skips the predicate.
type TenantRepo struct {
	tenantID string
	db       *gorm.DB
}

// ForTenant derives the caller's tenant from the session user and returns a
// repository bound to it and to the request context.
//
// It prefers the value RequireTenant stashed; when the middleware is absent
// from a chain it resolves (and caches) the tenant itself, so a route can
// never end up querying unscoped because a middleware was forgotten.
func ForTenant(c *gin.Context) (*TenantRepo, error) {
	if DB == nil {
		return nil, ErrAuthNotConfigured
	}
	if id := TenantFromContext(c); id != "" {
		return ForTenantID(c.Request.Context(), id), nil
	}
	v, ok := c.Get(authUserContextKey)
	if !ok {
		return nil, ErrNoTenant
	}
	user, _ := v.(*AuthUser)
	if user == nil {
		return nil, ErrNoTenant
	}
	id, err := ResolveTenant(c.Request.Context(), user.ID)
	if err != nil {
		return nil, err
	}
	c.Set(TenantContextKey, id)
	return ForTenantID(c.Request.Context(), id), nil
}

// ForTenantID binds a repository to an already-known firm id. This is the
// entry point for background work (the runner) where there is no
// gin.Context — the caller must have obtained the id from a tenant-scoped row,
// never from user input.
func ForTenantID(ctx context.Context, tenantID string) *TenantRepo {
	return &TenantRepo{tenantID: tenantID, db: DB.WithContext(ctx)}
}

// ForTenantTx binds a repository to a transaction someone else opened — the
// runner's lease-fenced commit — so a tool's or publisher's writes go through
// the same tenant predicate AND the same transaction as the step that accepts
// them. tenantID must come from the run row, never from model arguments.
func ForTenantTx(tx *gorm.DB, tenantID string) *TenantRepo {
	return &TenantRepo{tenantID: tenantID, db: tx}
}

// TenantID is the firm id every query below is pinned to. It is also the
// Casbin domain to enforce in, and the second segment of every S3 key.
func (r *TenantRepo) TenantID() string { return r.tenantID }

// scoped is the single place the tenant predicate is written. Every accessor
// goes through it; nothing else in the codebase may.
func (r *TenantRepo) scoped(model any) *gorm.DB {
	return r.db.Model(model).Where("tenant_id = ?", r.tenantID)
}

func (r *TenantRepo) Firms() *gorm.DB {
	// The firm row itself is keyed BY the tenant, not scoped by a column.
	return r.db.Model(&types.Firm{}).Where("id = ?", r.tenantID)
}

func (r *TenantRepo) Assets() *gorm.DB   { return r.scoped(&types.Asset{}) }
func (r *TenantRepo) Dossiers() *gorm.DB { return r.scoped(&types.Dossier{}) }
func (r *TenantRepo) Pieces() *gorm.DB   { return r.scoped(&types.Piece{}) }
func (r *TenantRepo) PieceParties() *gorm.DB {
	return r.scoped(&types.PiecePartie{})
}
func (r *TenantRepo) PieceChunks() *gorm.DB   { return r.scoped(&types.PieceChunk{}) }
func (r *TenantRepo) Conversations() *gorm.DB { return r.scoped(&types.Conversation{}) }
func (r *TenantRepo) Messages() *gorm.DB      { return r.scoped(&types.Message{}) }
func (r *TenantRepo) Runs() *gorm.DB          { return r.scoped(&types.Run{}) }
func (r *TenantRepo) RunSteps() *gorm.DB      { return r.scoped(&types.RunStep{}) }
func (r *TenantRepo) StepAttempts() *gorm.DB  { return r.scoped(&types.StepAttempt{}) }
func (r *TenantRepo) RunEvents() *gorm.DB     { return r.scoped(&types.RunEvent{}) }
func (r *TenantRepo) RunQuestions() *gorm.DB  { return r.scoped(&types.RunQuestion{}) }
func (r *TenantRepo) Artifacts() *gorm.DB     { return r.scoped(&types.Artifact{}) }
func (r *TenantRepo) Citations() *gorm.DB     { return r.scoped(&types.Citation{}) }
func (r *TenantRepo) LLMCalls() *gorm.DB      { return r.scoped(&types.LLMCall{}) }
func (r *TenantRepo) AuditEvents() *gorm.DB   { return r.scoped(&types.AuditEvent{}) }
func (r *TenantRepo) Skills() *gorm.DB        { return r.scoped(&types.Skill{}) }
func (r *TenantRepo) SkillVersions() *gorm.DB { return r.scoped(&types.SkillVersion{}) }
func (r *TenantRepo) MemoryPreferences() *gorm.DB {
	return r.scoped(&types.MemoryPreference{})
}

// Agent runtime tables.
func (r *TenantRepo) AgentTurns() *gorm.DB         { return r.scoped(&types.AgentTurn{}) }
func (r *TenantRepo) AgentReadReceipts() *gorm.DB  { return r.scoped(&types.AgentReadReceipt{}) }
func (r *TenantRepo) AgentEvidenceSpans() *gorm.DB { return r.scoped(&types.AgentEvidenceSpan{}) }
func (r *TenantRepo) AgentDocumentRevisions() *gorm.DB {
	return r.scoped(&types.AgentDocumentRevision{})
}
func (r *TenantRepo) AgentVerifications() *gorm.DB { return r.scoped(&types.AgentVerification{}) }
func (r *TenantRepo) AgentWebSources() *gorm.DB    { return r.scoped(&types.AgentWebSource{}) }

// RunStepDeps is the one private table with no tenant_id of its own (an edge
// carries no data). It is reachable only through run ids the caller has
// already read from a tenant-scoped query, and an empty list matches nothing
// rather than everything.
func (r *TenantRepo) RunStepDeps(runIDs []string) *gorm.DB {
	if len(runIDs) == 0 {
		return r.db.Model(&types.RunStepDep{}).Where("1 = 0")
	}
	return r.db.Model(&types.RunStepDep{}).Where("run_id IN ?", runIDs)
}

// Create inserts one row (or a slice of rows) with tenant_id stamped from the
// repository rather than from the caller's struct. The caller CANNOT write a
// row into the wrong tenant, even by mistake.
func (r *TenantRepo) Create(value any) error {
	if err := stampTenant(value, r.tenantID); err != nil {
		return err
	}
	return r.db.Create(value).Error
}

// Tx runs fn inside a transaction, with a repository bound to the same tenant
// and to the transaction. Use it wherever a half-applied change would be
// visible to an export — renumbering and final filenames commit together
// (§4.3).
func (r *TenantRepo) Tx(fn func(tx *TenantRepo) error) error {
	return r.db.Transaction(func(g *gorm.DB) error {
		return fn(&TenantRepo{tenantID: r.tenantID, db: g})
	})
}

// stampTenant writes tenantID into the TenantID field of a struct pointer or
// of every element of a slice pointer. A type without that field is a
// programming error: Create is for private rows only.
func stampTenant(value any, tenantID string) error {
	v := reflect.ValueOf(value)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("tenant repo: Create needs a non-nil pointer")
	}
	v = v.Elem()
	if v.Kind() == reflect.Slice {
		for i := 0; i < v.Len(); i++ {
			if err := stampTenantField(v.Index(i), tenantID); err != nil {
				return err
			}
		}
		return nil
	}
	return stampTenantField(v, tenantID)
}

func stampTenantField(v reflect.Value, tenantID string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return errors.New("tenant repo: Create got a nil element")
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("tenant repo: Create needs a struct, got %s", v.Kind())
	}
	f := v.FieldByName("TenantID")
	if !f.IsValid() || f.Kind() != reflect.String || !f.CanSet() {
		return fmt.Errorf("tenant repo: %s has no settable TenantID field — it is not a private table", v.Type())
	}
	f.SetString(tenantID)
	return nil
}
