// Package middlewares holds reusable Gin middleware (request id + log line,
// recovery extensions, etc.).
package middlewares

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	u "github.com/Arkel-ai/nomos/api/utils"
)

const RequestIDKey = "request_id"

var quietPaths = map[string]bool{
	"/api/health": true,
}

var log = u.NewLogger("app.request")

// RequestLog assigns each request a short id, exposes it as the
// X-Request-ID header, and emits one structured line per request:
//
//	HH:MM:SS LEVEL   app.request            <id> METHOD /path STATUS Xms
func RequestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := uuid.NewString()
		if len(id) >= 8 {
			id = id[:8]
		}
		c.Set(RequestIDKey, id)
		c.Header("X-Request-ID", id)

		start := time.Now()
		c.Next()
		ms := time.Since(start).Milliseconds()

		line := fmt.Sprintf("%s %s %s %d %dms",
			id, c.Request.Method, c.Request.URL.Path, c.Writer.Status(), ms)

		switch {
		case quietPaths[c.Request.URL.Path]:
			log.Debug(line)
		case c.Writer.Status() >= 500:
			log.Error(line)
		default:
			log.Info(line)
		}
	}
}

// RequestID returns the request id attached by RequestLog, or "" if absent.
func RequestID(c *gin.Context) string {
	if v, ok := c.Get(RequestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
