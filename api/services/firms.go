package services

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var firmsLog = u.NewLogger("services.firms")

// Tenancy (docs/PLAN.md §6.1, §14 sprint 0).
//
// The MVP has ONE firm, but tenancy is real from the first row: the firm id is
// the tenant_id column, the Casbin domain and the second segment of every S3
// key. The starter's DefaultDomain "*" is never used for Nomos data — a
// permission granted there is a permission in every tenant at once.

// BootstrapTenancy seeds the singleton firm and its role→permission matrix at
// boot, so a fresh database is usable without manual SQL and the Casbin domain
// exists before the first request. Idempotent, and safe to skip: tenant
// resolution provisions the same rows lazily on the first authenticated
// request (utils.ResolveTenant).
func BootstrapTenancy() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := u.EnsureFirm(ctx, u.DefaultFirmID, "Cabinet", ""); err != nil {
		if errors.Is(err, u.ErrAuthNotConfigured) {
			firmsLog.Info("Postgres not configured — tenancy will be seeded on first request")
			return
		}
		firmsLog.Warnf("Seeding the default firm failed: %v", err)
		return
	}
	firmsLog.Infof("Tenancy ready (firm=%s, casbin domain=%s)", u.DefaultFirmID, u.DefaultFirmID)
}

// AttachUserToFirm gives a user a role inside a firm's Casbin domain — the one
// operation that decides which tenant's rows a person can ever see. The
// invitation flow calls it once the Better Auth account exists.
func AttachUserToFirm(ctx context.Context, userID, firmID, role string) error {
	if role == "" {
		role = types.RoleUser
	}
	ok, err := u.FirmExists(ctx, firmID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("unknown firm")
	}
	if err := u.AddRoleForUserInDomain(userID, role, firmID); err != nil {
		return err
	}
	firmsLog.Infof("Attached a user to firm=%s with role=%s", firmID, role)
	return nil
}

// --- shared service helpers -------------------------------------------------

// tenantRepo is how every Nomos service starts. On failure it returns a nil
// repo plus the (status, body) the controller should return verbatim — there
// is no code path that continues without a tenant-scoped handle.
func tenantRepo(c *gin.Context) (*u.TenantRepo, int, any) {
	if u.DB == nil {
		return nil, http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	repo, err := u.ForTenant(c)
	if err != nil {
		if errors.Is(err, u.ErrAuthNotConfigured) {
			return nil, http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
		}
		if errors.Is(err, u.ErrNoTenant) {
			return nil, http.StatusUnauthorized, gin.H{"detail": "Not authenticated"}
		}
		firmsLog.Errorf("Tenant resolution failed: %v", err)
		return nil, http.StatusInternalServerError, gin.H{"detail": "Could not resolve tenant"}
	}
	return repo, 0, nil
}

// dossierObject is the Casbin object for per-dossier sharing (§10). It is
// always enforced in the firm's domain.
func dossierObject(id string) string { return "dossier:" + id }

// canAccessDossier reports whether the caller may act on one dossier. The
// creator always can; anyone else needs either a per-dossier grant or the
// role-level `dossier` permission inside THIS firm's domain.
//
// This is authorization, not isolation: a dossier from another tenant never
// reaches this function, because the query that loaded it was tenant-scoped.
func canAccessDossier(c *gin.Context, repo *u.TenantRepo, d types.Dossier, action string) bool {
	user := middlewares.CurrentUser(c)
	if user == nil {
		return false
	}
	if d.CreatedBy == user.ID {
		return true
	}
	if ok, err := u.Enforce(user.ID, repo.TenantID(), dossierObject(d.ID), action); err == nil && ok {
		return true
	}
	ok, err := u.Enforce(user.ID, repo.TenantID(), types.ObjectDossier, action)
	return err == nil && ok
}
