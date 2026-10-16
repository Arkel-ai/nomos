package services

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var blobsLog = u.NewLogger("services.blobs")

// ServeBlob backs GET /api/blobs/*key, the read side of the LOCAL blob
// backend (utils/blob.go). It exists only because there is no object store to
// presign against on a laptop: the local backend mints an app-relative,
// HMAC-signed, short-lived URL and the API serves the bytes itself, which
// keeps the semantics identical to S3 — a link that carries its own
// authorisation and expires.
//
// It is therefore registered ONLY when the resolved backend is local. With S3
// configured, downloads are real presigned URLs that never touch this process
// and this route does not exist.
//
// Authorisation is the signature and nothing else, exactly like a presigned
// URL: the caller proved possession of a link that a tenant-scoped,
// Casbin-checked endpoint issued to them minutes ago. The key is never logged.
func ServeBlob(c *gin.Context) {
	key := strings.TrimPrefix(c.Param("key"), "/")
	if err := u.VerifyBlobSignature(key, c.Query("expires"), c.Query("signature")); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"detail": "Invalid or expired link"})
		return
	}

	rc, err := u.Blob.Get(c.Request.Context(), key)
	if err != nil {
		switch {
		case errors.Is(err, u.ErrBlobNotFound):
			c.JSON(http.StatusNotFound, gin.H{"detail": "Object not found"})
		case errors.Is(err, u.ErrBlobKeyForbidden):
			c.JSON(http.StatusForbidden, gin.H{"detail": "Forbidden"})
		default:
			blobsLog.Errorf("Local blob read failed: %v", err)
			c.JSON(http.StatusBadGateway, gin.H{"detail": "Storage unavailable"})
		}
		return
	}
	defer rc.Close()

	c.Header("Content-Type", "application/octet-stream")
	if _, err := io.Copy(c.Writer, rc); err != nil {
		blobsLog.Warnf("Local blob stream interrupted: %v", err)
	}
}
