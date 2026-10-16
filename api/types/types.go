// Package types holds request / response shapes used by the controllers
// and services. Keep it free of business logic and HTTP machinery.
package types

// Role constants for the invitation / user-management feature. A deliberately
// small matrix: "admin" can manage users and invitations; "user" is a normal
// authenticated user. Extend IsAllowedRole and the Casbin policy seed
// (utils.SeedRolePolicies) together.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

var allowedRoles = map[string]bool{RoleAdmin: true, RoleUser: true}

// IsAllowedRole reports whether role is one the app manages. It guards the
// invite / set-role endpoints so callers can't grant arbitrary strings.
func IsAllowedRole(role string) bool { return allowedRoles[role] }

// Invitation is the GORM model behind the `invitations` table. It is
// app-owned data (Go migrates it) that drives the invite-only signup flow:
// an admin creates a pending row with a hashed one-time token; the invited
// user redeems it on the accept page. Better Auth still owns the resulting
// `user` row — this table only carries the intent + the role to grant.
type Invitation struct {
	ID         string `gorm:"primaryKey" json:"id"`
	Email      string `gorm:"index;not null" json:"email"`
	Role       string `gorm:"not null" json:"role"`
	TokenHash  string `gorm:"uniqueIndex;not null" json:"-"`          // sha256(raw token); raw is never stored
	Status     string `gorm:"not null;default:pending" json:"status"` // pending | accepted | revoked
	InvitedBy  string `json:"invited_by"`                             // Better Auth user id of the admin (or "bootstrap")
	AcceptedBy string `json:"accepted_by,omitempty"`                  // user id that redeemed it
	ExpiresAt  int64  `json:"expires_at"`                             // unix seconds
	CreatedAt  int64  `gorm:"autoCreateTime" json:"created_at"`
	AcceptedAt int64  `json:"accepted_at,omitempty"`
}

// InviteCreateRequest is the body for POST /invitations (admin only).
type InviteCreateRequest struct {
	Email string `json:"email" binding:"required,email"`
	Role  string `json:"role" binding:"required"`
}

// InviteAcceptRequest is the body for POST /invitations/accept. The caller
// is the just-created (and now authenticated) user redeeming their token.
type InviteAcceptRequest struct {
	Token string `json:"token" binding:"required"`
}

// SetUserRoleRequest is the body for PUT /admin/users/:id/role (admin only).
type SetUserRoleRequest struct {
	Role string `json:"role" binding:"required"`
}
