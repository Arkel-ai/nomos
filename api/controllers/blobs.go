package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
)

// ServeBlob → GET /blobs/*key. Registered only when the blob backend is the
// local filesystem one; see services/blobs.go.
func ServeBlob(c *gin.Context) {
	services.ServeBlob(c)
}
