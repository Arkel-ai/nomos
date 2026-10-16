package services

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Health is the liveness probe (GET /api/health). Keep this trivial — it must
// stay fast even under load.
func Health(c *gin.Context) (int, any) {
	return http.StatusOK, gin.H{"status": "ok"}
}
