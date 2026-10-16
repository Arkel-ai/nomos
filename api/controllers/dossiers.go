package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// CreateDossier → POST /dossiers
func CreateDossier(c *gin.Context) {
	var body types.DossierCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.CreateDossier(c, body))
}

// ListDossiers → GET /dossiers
func ListDossiers(c *gin.Context) {
	var q types.ListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ListDossiers(c, q))
}

// GetDossier → GET /dossiers/:id
func GetDossier(c *gin.Context) {
	c.JSON(services.GetDossier(c, c.Param("id")))
}

// DeleteDossier → DELETE /dossiers/:id
func DeleteDossier(c *gin.Context) {
	c.JSON(services.DeleteDossier(c, c.Param("id")))
}
