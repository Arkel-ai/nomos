package controllers

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
)

// ExportDossier → POST /dossiers/:id/export (zip + bordereau artifact). Every
// field is optional, so an empty body is valid and binds to the defaults.
func ExportDossier(c *gin.Context) {
	var body types.ExportRequest
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(services.ExportDossier(c, c.Param("id"), body))
}

// GET /runs/:id/artifacts is served by RunArtifacts in controllers/runs.go —
// it is a run-scoped read, and it authorizes through the run rather than
// through the artifact.

// DownloadArtifact → GET /artifacts/:id/download (presigned)
func DownloadArtifact(c *gin.Context) {
	c.JSON(services.DownloadArtifact(c, c.Param("id")))
}
