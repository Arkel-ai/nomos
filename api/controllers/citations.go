package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
)

// RunCitations → GET /runs/:id/citations?document_revision_id=&message_id=&after=&limit=
func RunCitations(c *gin.Context) {
	var q services.CitationListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListRunCitations(c, c.Param("id"), q))
}

// GetCitation → GET /citations/:id (citation + bounded source passage)
func GetCitation(c *gin.Context) {
	c.JSON(services.GetCitation(c, c.Param("id")))
}

// RunCoverage → GET /runs/:id/coverage (reading coverage of the run's scope)
func RunCoverage(c *gin.Context) {
	c.JSON(services.RunCoverage(c, c.Param("id")))
}
