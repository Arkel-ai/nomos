package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
)

// IngestPieces → POST /dossiers/:id/pieces (multipart, n files).
//
// The whole ingestion pipeline lives in services/ingest.go: magic-byte
// sniffing, the pre-parse hash, the encrypted-PDF refusal, the zip-bomb
// bounds, the untouched original in object storage, and the run that extracts,
// OCRs, chunks and qualifies. The controller only hands over the dossier id —
// a partial batch comes back as {accepted, rejected[{name, reason}]}.
func IngestPieces(c *gin.Context) {
	c.JSON(services.IngestPieces(c, c.Param("id")))
}
