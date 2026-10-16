package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// ListUsers → GET /admin/users (admin:read). Powers the admin user panel.
func ListUsers(c *gin.Context) {
	c.JSON(services.ListUsers(c))
}

// SetUserRole → PUT /admin/users/:id/role (admin:write). Changes a user's
// permission level.
func SetUserRole(c *gin.Context) {
	var body types.SetUserRoleRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.SetUserRole(c, c.Param("id"), body))
}
