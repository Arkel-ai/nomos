package services

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var adminLog = u.NewLogger("services.admin")

// ListUsers returns every Better Auth user with their Casbin roles. It
// reads the `user` table (Better Auth owns it; we only read) and joins the
// roles from the enforcer. Powers the admin user panel. admin:read.
func ListUsers(c *gin.Context) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	var users []u.BetterAuthUser
	if err := u.DB.WithContext(c.Request.Context()).Order("email").Find(&users).Error; err != nil {
		adminLog.Errorf("list users failed: %v", err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to list users"}
	}
	out := make([]gin.H, 0, len(users))
	for _, usr := range users {
		out = append(out, gin.H{
			"id":             usr.ID,
			"email":          usr.Email,
			"name":           usr.Name,
			"email_verified": usr.EmailVerified,
			"roles":          u.GetRolesForUser(usr.ID, u.DefaultDomain),
		})
	}
	return http.StatusOK, gin.H{"users": out}
}

// SetUserRole replaces a user's role (the "change a user's permission"
// action). Guards against an admin demoting themselves so the last admin
// can't be locked out by accident. admin:write.
func SetUserRole(c *gin.Context, userID string, body types.SetUserRoleRequest) (int, any) {
	if u.Enforcer == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Authorization not configured"}
	}
	role := strings.ToLower(strings.TrimSpace(body.Role))
	if !types.IsAllowedRole(role) {
		return http.StatusBadRequest, gin.H{"detail": "Invalid role"}
	}
	actor := middlewares.CurrentUser(c)
	if actor.ID == userID && role != types.RoleAdmin {
		return http.StatusBadRequest, gin.H{"detail": "You cannot remove your own admin role"}
	}

	if role == types.RoleAdmin {
		if err := u.SeedRolePolicies(); err != nil {
			adminLog.Errorf("seed admin policy failed: %v", err)
			return http.StatusInternalServerError, gin.H{"detail": "Failed to set role"}
		}
	}
	if err := u.SetUserRole(userID, role, u.DefaultDomain); err != nil {
		adminLog.Errorf("set role failed (user=%s role=%s): %v", userID, role, err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to set role"}
	}
	adminLog.Infof("Set role=%s for user=%s by=%s", role, userID, actor.ID)
	return http.StatusOK, gin.H{"user_id": userID, "role": role}
}
