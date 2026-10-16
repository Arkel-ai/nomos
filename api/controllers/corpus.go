package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// SyncCorpus → POST /admin/corpus/sync
func SyncCorpus(c *gin.Context) {
	var body types.CorpusSyncRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.SyncCorpus(c, body))
}

// CorpusStatus → GET /admin/corpus/status
func CorpusStatus(c *gin.Context) {
	c.JSON(services.CorpusStatus(c))
}
