package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// ListPieces → GET /dossiers/:id/pieces
func ListPieces(c *gin.Context) {
	var q types.ListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListPieces(c, c.Param("id"), q))
}

// UpdatePiece → PATCH /pieces/:id (lawyer correction)
func UpdatePiece(c *gin.Context) {
	var body types.PieceUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.UpdatePiece(c, c.Param("id"), body))
}

// DownloadPiece → GET /pieces/:id/download (presigned)
func DownloadPiece(c *gin.Context) {
	c.JSON(services.DownloadPiece(c, c.Param("id")))
}
