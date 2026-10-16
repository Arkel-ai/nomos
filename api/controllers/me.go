package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
)

// Me → GET /me. Wire this route behind middlewares.RequireAuth() in
func Me(c *gin.Context) {
	c.JSON(services.Me(c))
}
