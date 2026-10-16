package utils

import (
	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	gormadapter "github.com/casbin/gorm-adapter/v3"

	casbinmodel "github.com/Arkel-ai/nomos/api/casbin"
)

var casbinLog = NewLogger("utils.casbin")

// Enforcer is the package-level Casbin enforcer. It is nil when Postgres
// isn't configured (no DATABASE_URL) — authz middleware then returns 503,
// matching the rest of the optional-integration contract.
var Enforcer *casbin.Enforcer

// DefaultDomain is the tenant used by the starter's example routes. Real
// apps key the domain on an org / workspace id pulled from the request.
const DefaultDomain = "*"

// initCasbin wires the enforcer to the same GORM connection used by the
// app. The gorm adapter auto-creates and manages the `casbin_rule`
// table, so Casbin policies live in Postgres alongside the app data and
// survive restarts. Must be called after initDB.
func initCasbin() {
	if DB == nil {
		casbinLog.Info("Postgres not configured — Casbin authz endpoints will return 503")
		return
	}
	m, err := model.NewModelFromString(casbinmodel.Model)
	if err != nil {
		casbinLog.Warnf("Casbin model parse failed: %v", err)
		return
	}
	adapter, err := gormadapter.NewAdapterByDB(DB)
	if err != nil {
		casbinLog.Warnf("Casbin gorm adapter init failed: %v", err)
		return
	}
	e, err := casbin.NewEnforcer(m, adapter)
	if err != nil {
		casbinLog.Warnf("Casbin enforcer init failed: %v", err)
		return
	}
	if err := e.LoadPolicy(); err != nil {
		casbinLog.Warnf("Casbin LoadPolicy failed: %v", err)
		return
	}
	Enforcer = e
	casbinLog.Info("Casbin enforcer initialized (Postgres-backed policies)")
}

// Enforce reports whether sub may perform act on obj within dom. Returns
// false when the enforcer isn't configured (callers / middleware decide
// how to surface that — typically 503).
func Enforce(sub, dom, obj, act string) (bool, error) {
	if Enforcer == nil {
		return false, ErrAuthNotConfigured
	}
	return Enforcer.Enforce(sub, dom, obj, act)
}

// AddRoleForUserInDomain grants `role` to `user` within `domain` and
// persists it to Postgres. Idempotent. Example helper used by the
// POST /roles reference endpoint.
func AddRoleForUserInDomain(user, role, domain string) error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	if _, err := Enforcer.AddGroupingPolicy(user, role, domain); err != nil {
		return err
	}
	return Enforcer.SavePolicy()
}

// GrantPermission gives `sub` (a user id or a role) permission to `act` on
// `obj` within `domain`, persisting it to Postgres. Idempotent. Used by the
// file-sharing example to grant a specific user read access to one object
// (obj = "file:<key>"), so per-file authorization is a single call.
func GrantPermission(sub, domain, obj, act string) error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	has, err := Enforcer.HasPolicy(sub, domain, obj, act)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := Enforcer.AddPolicy(sub, domain, obj, act); err != nil {
		return err
	}
	return Enforcer.SavePolicy()
}

// GetRolesForUser returns the roles granted to a user within a domain.
// Returns nil when the enforcer isn't configured. Used by the user panel
// and GET /me to surface a caller's roles (e.g. to gate admin-only UI).
func GetRolesForUser(user, domain string) []string {
	if Enforcer == nil {
		return nil
	}
	return Enforcer.GetRolesForUserInDomain(user, domain)
}

// SetUserRole replaces a user's roles in a domain with exactly `role`
// (clears any existing roles first), then persists to Postgres. This is
// the "change a user's permission" primitive behind the admin panel —
// distinct from AddRoleForUserInDomain, which is additive.
func SetUserRole(user, role, domain string) error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	for _, existing := range Enforcer.GetRolesForUserInDomain(user, domain) {
		if _, err := Enforcer.DeleteRoleForUserInDomain(user, existing, domain); err != nil {
			return err
		}
	}
	if _, err := Enforcer.AddGroupingPolicy(user, role, domain); err != nil {
		return err
	}
	return Enforcer.SavePolicy()
}

// SeedRolePolicies seeds the starter's permission matrix so a fresh
// database is usable end-to-end without manual SQL: the "admin" role can
// read and write the "admin" object (which gates the user panel,
// invitation, and set-role endpoints). Idempotent. The "user" role is
// intentionally unprivileged at the authz layer — extend this as the app
// grows its resource matrix.
func SeedRolePolicies() error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	for _, act := range []string{"read", "write"} {
		if err := EnsureRolePolicy("admin", DefaultDomain, "admin", act); err != nil {
			return err
		}
	}
	return nil
}

// EnsureRolePolicy makes sure a role has a permission rule (p line). It
// is a convenience for seeding the example "admin can read admin" rule
// so the starter works end-to-end without manual SQL.
func EnsureRolePolicy(role, domain, obj, act string) error {
	if Enforcer == nil {
		return ErrAuthNotConfigured
	}
	has, err := Enforcer.HasPolicy(role, domain, obj, act)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := Enforcer.AddPolicy(role, domain, obj, act); err != nil {
		return err
	}
	return Enforcer.SavePolicy()
}
