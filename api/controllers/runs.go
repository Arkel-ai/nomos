package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// GetRun → GET /runs/:id (plan + steps + questions)
func GetRun(c *gin.Context) {
	c.JSON(services.GetRun(c, c.Param("id")))
}

// RunEvents → GET /runs/:id/events?after=&limit=
func RunEvents(c *gin.Context) {
	var q types.EventsQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.RunEvents(c, c.Param("id"), q))
}

// AnswerRun → POST /runs/:id/answers (HITL; skip is an answer)
func AnswerRun(c *gin.Context) {
	var body types.AnswersRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.AnswerRun(c, c.Param("id"), body))
}

// CancelRun → POST /runs/:id/cancel
func CancelRun(c *gin.Context) {
	c.JSON(services.CancelRun(c, c.Param("id")))
}

// RunArtifacts → GET /runs/:id/artifacts
func RunArtifacts(c *gin.Context) {
	c.JSON(services.RunArtifacts(c, c.Param("id")))
}
