package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var inviteLog = u.NewLogger("services.invitations")

// inviteTTL is how long an invitation stays redeemable.
const inviteTTL = 7 * 24 * time.Hour

// appBaseURL / bootstrapAdminEmail are configured at boot from config. They
// are package state (not threaded through gin.Context) because the handlers
// only receive the request body.
var (
	appBaseURL          = "http://localhost:5173"
	bootstrapAdminEmail string
)

// ConfigureInvitations wires boot-time config into the invitation service.
// Call once from main after InitClients.
func ConfigureInvitations(cfg config.Config) {
	if cfg.AppBaseURL != "" {
		appBaseURL = strings.TrimRight(cfg.AppBaseURL, "/")
	}
	bootstrapAdminEmail = strings.ToLower(strings.TrimSpace(cfg.BootstrapAdminEmail))
}

// --- token helpers ---------------------------------------------------------

func genToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func genID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func inviteLink(raw string) string {
	return fmt.Sprintf("%s/accept-invite?token=%s", appBaseURL, raw)
}

// --- handlers --------------------------------------------------------------

// CreateInvitation creates a pending invitation for an email + role and
// returns the one-time accept link. Wired behind RequireAuth() +
// RequirePermission("admin", "write").
func CreateInvitation(c *gin.Context, body types.InviteCreateRequest) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	role := strings.ToLower(strings.TrimSpace(body.Role))
	if !types.IsAllowedRole(role) {
		return http.StatusBadRequest, gin.H{"detail": "Invalid role"}
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))

	var existing u.BetterAuthUser
	if err := u.DB.WithContext(c.Request.Context()).Where("lower(email) = ?", email).First(&existing).Error; err == nil {
		return http.StatusConflict, gin.H{"detail": "A user with this email already exists"}
	}

	// Supersede any prior pending invite for this email so only the newest
	// token is redeemable.
	u.DB.WithContext(c.Request.Context()).Model(&types.Invitation{}).
		Where("lower(email) = ? AND status = ?", email, "pending").
		Update("status", "revoked")

	raw, hash, err := genToken()
	if err != nil {
		inviteLog.Errorf("token generation failed: %v", err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to create invitation"}
	}

	actor := middlewares.CurrentUser(c)
	inv := types.Invitation{
		ID:        genID(),
		Email:     email,
		Role:      role,
		TokenHash: hash,
		Status:    "pending",
		InvitedBy: actor.ID,
		ExpiresAt: time.Now().Add(inviteTTL).Unix(),
	}
	if err := u.DB.WithContext(c.Request.Context()).Create(&inv).Error; err != nil {
		inviteLog.Errorf("invitation insert failed (email=%s): %v", email, err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to create invitation"}
	}

	link := inviteLink(raw)
	SendInvite(email, link)
	inviteLog.Infof("Invitation created (email=%s role=%s by=%s)", email, role, actor.ID)
	return http.StatusOK, gin.H{
		"id":         inv.ID,
		"email":      inv.Email,
		"role":       inv.Role,
		"status":     inv.Status,
		"expires_at": inv.ExpiresAt,
		"link":       link,
	}
}

// ListInvitations returns every invitation, newest first. The token hash is
// never serialized (json:"-"). admin:read.
func ListInvitations(c *gin.Context) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	var invs []types.Invitation
	if err := u.DB.WithContext(c.Request.Context()).Order("created_at desc").Find(&invs).Error; err != nil {
		inviteLog.Errorf("list invitations failed: %v", err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to list invitations"}
	}
	return http.StatusOK, gin.H{"invitations": invs}
}

// ValidateInvitation reports whether a raw token maps to a redeemable
// invitation, and (when valid) the email it was issued to so the accept
// page can prefill it. Public — no auth: the token is the secret.
func ValidateInvitation(c *gin.Context, token string) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	if token == "" {
		return http.StatusOK, gin.H{"valid": false, "reason": "missing"}
	}
	var inv types.Invitation
	if err := u.DB.WithContext(c.Request.Context()).Where("token_hash = ?", hashToken(token)).First(&inv).Error; err != nil {
		return http.StatusOK, gin.H{"valid": false, "reason": "not_found"}
	}
	if inv.Status != "pending" {
		return http.StatusOK, gin.H{"valid": false, "reason": inv.Status}
	}
	if time.Now().Unix() > inv.ExpiresAt {
		return http.StatusOK, gin.H{"valid": false, "reason": "expired"}
	}
	return http.StatusOK, gin.H{"valid": true, "email": inv.Email, "role": inv.Role}
}

// RevokeInvitation marks a pending invitation revoked. admin:write.
func RevokeInvitation(c *gin.Context, id string) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	res := u.DB.WithContext(c.Request.Context()).Model(&types.Invitation{}).
		Where("id = ? AND status = ?", id, "pending").
		Update("status", "revoked")
	if res.Error != nil {
		inviteLog.Errorf("revoke invitation failed (id=%s): %v", id, res.Error)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to revoke invitation"}
	}
	if res.RowsAffected == 0 {
		return http.StatusNotFound, gin.H{"detail": "No pending invitation with that id"}
	}
	inviteLog.Infof("Invitation revoked (id=%s)", id)
	return http.StatusOK, gin.H{"id": id, "status": "revoked"}
}

// AcceptInvitation redeems a token for the currently-authenticated user.
// The caller has just created their Better Auth account (signup is gated to
// invited emails by the front), so we bind the role to *this* user only if
// their email matches the invitation. Wired behind RequireAuth().
func AcceptInvitation(c *gin.Context, body types.InviteAcceptRequest) (int, any) {
	if u.DB == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Database not configured"}
	}
	if u.Enforcer == nil {
		return http.StatusServiceUnavailable, gin.H{"detail": "Authorization not configured"}
	}
	user := middlewares.CurrentUser(c)

	var inv types.Invitation
	if err := u.DB.WithContext(c.Request.Context()).Where("token_hash = ?", hashToken(body.Token)).First(&inv).Error; err != nil {
		return http.StatusNotFound, gin.H{"detail": "Invalid invitation"}
	}
	if inv.Status != "pending" {
		return http.StatusConflict, gin.H{"detail": "Invitation already " + inv.Status}
	}
	if time.Now().Unix() > inv.ExpiresAt {
		return http.StatusGone, gin.H{"detail": "Invitation expired"}
	}
	if !strings.EqualFold(inv.Email, user.Email) {
		inviteLog.Warnf("invite email mismatch (invite=%s user=%s)", inv.Email, user.Email)
		return http.StatusForbidden, gin.H{"detail": "Invitation email does not match the signed-in user"}
	}

	if inv.Role == types.RoleAdmin {
		if err := u.SeedRolePolicies(); err != nil {
			inviteLog.Errorf("seed admin policy failed: %v", err)
			return http.StatusInternalServerError, gin.H{"detail": "Failed to grant role"}
		}
	}
	if err := u.SetUserRole(user.ID, inv.Role, u.DefaultDomain); err != nil {
		inviteLog.Errorf("set role failed (user=%s role=%s): %v", user.ID, inv.Role, err)
		return http.StatusInternalServerError, gin.H{"detail": "Failed to grant role"}
	}

	// Finalize on a detached context: the role grant above already succeeded,
	// so a client disconnect must not leave the invitation redeemable.
	now := time.Now().Unix()
	u.DB.WithContext(context.WithoutCancel(c.Request.Context())).Model(&types.Invitation{}).Where("id = ?", inv.ID).Updates(map[string]any{
		"status":      "accepted",
		"accepted_by": user.ID,
		"accepted_at": now,
	})
	inviteLog.Infof("Invitation accepted (email=%s role=%s user=%s)", inv.Email, inv.Role, user.ID)
	return http.StatusOK, gin.H{"role": inv.Role}
}

// --- bootstrap -------------------------------------------------------------

// BootstrapAdmin ensures the configured first admin holds the "admin" role.
// The Better Auth account (with its password) is created by the front Node
// server on its boot; here we only grant the Casbin role once that user
// row exists. If the account hasn't been created yet (boot ordering), the
// grant happens lazily on the admin's first authenticated request — see
// MaybeGrantBootstrapAdmin. Call once from main after InitClients.
func BootstrapAdmin() {
	if bootstrapAdminEmail == "" || u.DB == nil || u.Enforcer == nil {
		return
	}
	var usr u.BetterAuthUser
	if err := u.DB.Where("lower(email) = ?", bootstrapAdminEmail).First(&usr).Error; err != nil {
		inviteLog.Infof("Bootstrap admin %s not present yet — role will be granted on first sign-in", bootstrapAdminEmail)
		return
	}
	ensureBootstrapAdminRole(usr.ID)
}

// MaybeGrantBootstrapAdmin grants the admin role to the bootstrap admin the
// first time they hit an authenticated endpoint (covers the case where the
// front created the account after the Go API booted). No-op otherwise.
func MaybeGrantBootstrapAdmin(user *u.AuthUser) {
	if bootstrapAdminEmail == "" || user == nil || u.Enforcer == nil {
		return
	}
	if !strings.EqualFold(user.Email, bootstrapAdminEmail) {
		return
	}
	for _, r := range u.GetRolesForUser(user.ID, u.DefaultDomain) {
		if r == types.RoleAdmin {
			return
		}
	}
	ensureBootstrapAdminRole(user.ID)
}

func ensureBootstrapAdminRole(userID string) {
	if err := u.SeedRolePolicies(); err != nil {
		inviteLog.Errorf("bootstrap: seed admin policy failed: %v", err)
		return
	}
	if err := u.AddRoleForUserInDomain(userID, types.RoleAdmin, u.DefaultDomain); err != nil {
		inviteLog.Errorf("bootstrap: grant admin role failed (user=%s): %v", userID, err)
		return
	}
	inviteLog.Infof("Bootstrap admin role ensured for user=%s", userID)
}
