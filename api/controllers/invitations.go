package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// CreateInvitation → POST /invitations
//
// Admin-only (RequireAuth + RequirePermission("admin", "write")). Returns
// the one-time accept link in the response so the UI can show/copy it.
func CreateInvitation(c *gin.Context) {
	var body types.InviteCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.CreateInvitation(c, body))
}

// ListInvitations → GET /invitations (admin:read).
func ListInvitations(c *gin.Context) {
	c.JSON(services.ListInvitations(c))
}

// ValidateInvitation → GET /invitations/validate?token=... (public).
func ValidateInvitation(c *gin.Context) {
	c.JSON(services.ValidateInvitation(c, c.Query("token")))
}

// RevokeInvitation → DELETE /invitations/:id (admin:write).
func RevokeInvitation(c *gin.Context) {
	c.JSON(services.RevokeInvitation(c, c.Param("id")))
}

// AcceptInvitation → POST /invitations/accept (RequireAuth).
//
// The just-created, now-authenticated user redeems their token; the service
// verifies the invitation email matches the session user before granting
// the role.
func AcceptInvitation(c *gin.Context) {
	var body types.InviteAcceptRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.AcceptInvitation(c, body))
}
