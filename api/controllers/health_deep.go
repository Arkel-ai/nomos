package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/services"
)

// HealthProbe → GET /health. Without ?deep=1 it is the template's trivial
// liveness probe plus the public feature flags. With ?deep=1 it is a real
// readiness signal that actually talks to Postgres, S3 and the LLM gateway,
// and answers 503 when a required dependency is down (docs/PLAN.md §2.5).
//
// The two live on one path deliberately: the platform can be pointed at
// `/api/health?deep=1` for readiness without the frontend's boot call
// changing, and there is no second probe to keep in sync.
func HealthProbe(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Query("deep") != "" && c.Query("deep") != "0" {
			c.JSON(services.HealthDeep(c, cfg))
			return
		}
		c.JSON(services.Health(c))
	}
}
