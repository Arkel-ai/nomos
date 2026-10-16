package middlewares

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	u "github.com/Arkel-ai/nomos/api/utils"
)

// AuthUserKey is the gin.Context key under which RequireAuth /
// OptionalAuth stash the verified *utils.AuthUser. Read it via
// CurrentUser(c).
const AuthUserKey = "auth_user"

// betterAuthCookie is the default Better Auth session cookie name. The
// frontend (adapter-node) sets it on login; same-origin requests carry
// it automatically, so the SPA does not need to attach a header.
//
// In production (https, or NODE_ENV=production) Better Auth prefixes the
// cookie with "__Secure-", so the browser sends
// "__Secure-better-auth.session_token"; dev (http) uses the bare name. We
// accept both, mirroring Better Auth's own cookie reader.
const betterAuthCookie = "better-auth.session_token"
const betterAuthSecureCookie = "__Secure-" + betterAuthCookie

var authLog = u.NewLogger("middlewares.auth")

// RequireAuth aborts the request with 401 unless the caller presents a
// valid Better Auth session — either as `Authorization: Bearer <token>`
// or the `better-auth.session_token` cookie. Validation is a lookup in
// the shared Postgres `session` table (Better Auth is the source of
// truth); the API never signs or verifies a JWT itself.
//
// Returns 503 (not 401) when Postgres isn't configured, so the operator
// can tell server misconfiguration from a client error.
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := sessionToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"detail": "Missing session token"})
			return
		}
		user, err := u.GetUserFromSession(token)
		if err != nil {
			if errors.Is(err, u.ErrAuthNotConfigured) {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable,
					gin.H{"detail": "Database not configured"})
				return
			}
			authLog.Debugf("Session rejected: %v", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"detail": "Invalid or expired session"})
			return
		}
		c.Set(AuthUserKey, user)
		c.Next()
	}
}

// OptionalAuth attaches the user to the context when a valid session is
// present, and never aborts. Use it on endpoints that change behaviour
// based on auth but still serve anonymous traffic.
func OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if token := sessionToken(c); token != "" {
			if user, err := u.GetUserFromSession(token); err == nil {
				c.Set(AuthUserKey, user)
			}
		}
		c.Next()
	}
}

// CurrentUser returns the verified user attached by RequireAuth /
// OptionalAuth, or nil if none. Services use it like:
//
//	if user := middlewares.CurrentUser(c); user != nil { … }
func CurrentUser(c *gin.Context) *u.AuthUser {
	v, ok := c.Get(AuthUserKey)
	if !ok {
		return nil
	}
	user, _ := v.(*u.AuthUser)
	return user
}

// sessionToken pulls the Better Auth session token from the Bearer
// header first, then the session cookie.
func sessionToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		if t := strings.TrimSpace(h[len(prefix):]); t != "" {
			return t
		}
	}
	for _, name := range []string{betterAuthCookie, betterAuthSecureCookie} {
		if ck, err := c.Cookie(name); err == nil {
			if t := strings.TrimSpace(ck); t != "" {
				return t
			}
		}
	}
	return ""
}
