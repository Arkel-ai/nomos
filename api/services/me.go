package services

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/middlewares"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var meLog = u.NewLogger("services.me")

// Me returns the calling user's profile. The auth middleware (configured
// in main.go) has already validated the Better Auth session and attached
// the user, so this service is just a projection.
//
// This is the "minimal protected endpoint" example. Copy it as a
// starting point for any route that needs an authenticated Better Auth
// session (Bearer token or session cookie).
func Me(c *gin.Context) (int, any) {
	user := middlewares.CurrentUser(c)
	if user == nil {
		// Should never happen when wired behind RequireAuth(), but we
		// defend in depth so a misconfigured route can't leak.
		return http.StatusUnauthorized, gin.H{"detail": "Not authenticated"}
	}
	// Grant the bootstrap admin its role on first authenticated request
	// (covers the boot-ordering case where the front created the account
	// after the Go API started). No-op for everyone else.
	MaybeGrantBootstrapAdmin(user)

	roles := u.GetRolesForUser(user.ID, u.DefaultDomain)
	isAdmin := false
	for _, r := range roles {
		if r == "admin" {
			isAdmin = true
			break
		}
	}

	meLog.Debugf("Me requested (user_id=%s roles=%v)", user.ID, roles)
	return http.StatusOK, gin.H{
		"id":             user.ID,
		"email":          user.Email,
		"name":           user.Name,
		"email_verified": user.EmailVerified,
		"roles":          roles,
		"is_admin":       isAdmin,
	}
}
