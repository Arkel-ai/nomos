package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// CreateConversation → POST /conversations (optionally with the first message)
func CreateConversation(c *gin.Context) {
	var body types.ConversationCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.CreateConversation(c, body))
}

// ListConversations → GET /conversations (the history rail)
func ListConversations(c *gin.Context) {
	var q types.ListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListConversations(c, q))
}

// GetConversation → GET /conversations/:id (messages + runs + artifacts)
func GetConversation(c *gin.Context) {
	c.JSON(services.GetConversation(c, c.Param("id")))
}

// UpdateConversation → PATCH /conversations/:id (rename, bind/unbind dossier)
func UpdateConversation(c *gin.Context) {
	var body types.ConversationUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.UpdateConversation(c, c.Param("id"), body))
}

// DeleteConversation → DELETE /conversations/:id
func DeleteConversation(c *gin.Context) {
	c.JSON(services.DeleteConversation(c, c.Param("id")))
}

// PostMessage → POST /conversations/:id/messages (may start a run)
func PostMessage(c *gin.Context) {
	var body types.MessageCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.PostMessage(c, c.Param("id"), body))
}

// ConversationEvents → GET /conversations/:id/events?after=&limit=
func ConversationEvents(c *gin.Context) {
	var q types.EventsQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ConversationEvents(c, c.Param("id"), q))
}

// ConversationStream → GET /conversations/:id/stream?after= (SSE). The service
// writes directly to the response, so this is a one-line delegate.
func ConversationStream(c *gin.Context) {
	var q types.EventsQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	services.ConversationStream(c, c.Param("id"), q)
}
