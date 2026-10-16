package middlewares

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var authzLog = u.NewLogger("middlewares.authz")

// RequirePermission gates a route on a Casbin rule. It MUST run after
// RequireAuth() — it reads the authenticated user via CurrentUser and
// asks the enforcer whether that user may `act` on `obj` in the default
// domain.
//
//	api.GET("/admin/users",
//	    middlewares.RequireAuth(),
//	    middlewares.RequirePermission("admin", "read"),
//	    controllers.ListUsers)
//
// 403 on deny, 401 if no user (misconfigured chain), 503 if the enforcer
// isn't configured (no DATABASE_URL).
func RequirePermission(obj, act string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := CurrentUser(c)
		if user == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"detail": "Not authenticated"})
			return
		}
		ok, err := u.Enforce(user.ID, u.DefaultDomain, obj, act)
		if err != nil {
			if errors.Is(err, u.ErrAuthNotConfigured) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable,
					gin.H{"detail": "Authorization not configured"})
				return
			}
			authzLog.Errorf("Casbin enforce failed (user=%s obj=%s act=%s): %v", user.ID, obj, act, err)
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				gin.H{"detail": "Authorization check failed"})
			return
		}
		if !ok {
			authzLog.Debugf("Denied user=%s obj=%s act=%s", user.ID, obj, act)
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"detail": "Forbidden"})
			return
		}
		c.Next()
	}
}
