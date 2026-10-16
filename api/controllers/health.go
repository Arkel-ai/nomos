// Package controllers exposes thin HTTP handlers. Each function binds the
// request body, delegates to a service, and returns the result via
// `c.JSON(...)`. Business logic lives in `services/`.
package controllers

import (
	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/services"
)

// Health is the liveness probe.
func Health(c *gin.Context) {
	c.JSON(services.Health(c))
}
