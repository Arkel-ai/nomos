package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// ListMemory → GET /memory
func ListMemory(c *gin.Context) {
	var q types.ListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListMemory(c, q))
}

// UpsertMemory → POST /memory
func UpsertMemory(c *gin.Context) {
	var body types.MemoryUpsertRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.UpsertMemory(c, body))
}

// DeleteMemory → DELETE /memory/:id
func DeleteMemory(c *gin.Context) {
	c.JSON(services.DeleteMemory(c, c.Param("id")))
}
