package middlewares

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var tenantLog = u.NewLogger("middlewares.tenant")

// Tenancy for the request chain (docs/PLAN.md §6.1).
//
// One value, resolved once, read by both halves of the defence:
//
//	authorization → RequireTenantPermission enforces in the tenant's Casbin
//	                domain (the FIRM ID, never the starter's "*", which would
//	                erase tenancy);
//	data access   → utils.ForTenant returns a repository pinned to the same id.
//
// If those two ever disagreed about which tenant a request belongs to, a
// caller could be authorized in one firm and served rows from another. They
// cannot disagree here because they read the same context key.

// RequireTenant resolves the caller's firm and stashes it on the context. It
// MUST run after RequireAuth().
//
//	api.GET("/dossiers",
//	    middlewares.RequireAuth(),
//	    middlewares.RequireTenant(),
//	    middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
//	    controllers.ListDossiers)
//
// 401 without a session, 503 when Postgres/Casbin are not configured.
func RequireTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := resolveTenant(c); !ok {
			return
		}
		c.Next()
	}
}

// RequireTenantPermission is RequirePermission with the domain set to the
// caller's firm id instead of utils.DefaultDomain. Every Nomos route that
// touches private data must use this one: a policy evaluated in the "*" domain
// says nothing about which tenant the actor may act in.
//
// It resolves the tenant itself when RequireTenant did not run, so the gate
// can never silently degrade to the global domain.
func RequireTenantPermission(obj, act string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := CurrentUser(c)
		if user == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"detail": "Not authenticated"})
			return
		}
		domain, ok := resolveTenant(c)
		if !ok {
			return
		}
		allowed, err := u.Enforce(user.ID, domain, obj, act)
		if err != nil {
			if errors.Is(err, u.ErrAuthNotConfigured) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable,
					gin.H{"detail": "Authorization not configured"})
				return
			}
			tenantLog.Errorf("Casbin enforce failed (obj=%s act=%s domain=%s): %v", obj, act, domain, err)
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				gin.H{"detail": "Authorization check failed"})
			return
		}
		if !allowed {
			tenantLog.Debugf("Denied user=%s obj=%s act=%s domain=%s", user.ID, obj, act, domain)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"detail": "Forbidden"})
			return
		}
		c.Next()
	}
}

// TenantID returns the firm id resolved for this request, or "" if the chain
// has not resolved one. Services should prefer utils.ForTenant, which returns
// a repository that cannot be used without the predicate.
func TenantID(c *gin.Context) string { return u.TenantFromContext(c) }

// resolveTenant resolves-and-caches the firm id, aborting the request with the
// right status when it cannot. The bool is false once the response is written.
func resolveTenant(c *gin.Context) (string, bool) {
	if id := u.TenantFromContext(c); id != "" {
		return id, true
	}
	user := CurrentUser(c)
	if user == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "Not authenticated"})
		return "", false
	}
	id, err := u.ResolveTenant(c.Request.Context(), user.ID)
	if err != nil {
		if errors.Is(err, u.ErrAuthNotConfigured) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable,
				gin.H{"detail": "Database not configured"})
			return "", false
		}
		tenantLog.Errorf("Tenant resolution failed for user=%s: %v", user.ID, err)
		c.AbortWithStatusJSON(http.StatusInternalServerError,
			gin.H{"detail": "Could not resolve tenant"})
		return "", false
	}
	c.Set(u.TenantContextKey, id)
	return id, true
}
