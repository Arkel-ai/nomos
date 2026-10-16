package middlewares

import (
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	u "github.com/Arkel-ai/nomos/api/utils"
)

var rateLog = u.NewLogger("middlewares.ratelimit")

// RateLimit caps how many requests one authenticated user may make to a
// route per minute. It MUST run after RequireAuth() — the bucket is keyed
// by user id, so a request without a verified user is refused.
//
//	api.POST("/search",
//	    middlewares.RequireAuth(),
//	    middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
//	    controllers.Search)
//
// Put it on every route that spends money downstream (LLM calls, paid
// third-party APIs): the session stops strangers, the limit stops one
// logged-in account from draining the budget.
//
// Buckets are a token bucket held in memory, so the effective limit is
// per API pod. That is deliberate for the template — replace the map with
// Redis if you run several replicas and need an exact global limit.
func RateLimit(perMinute int) gin.HandlerFunc {
	var (
		mu      sync.Mutex
		buckets = map[string]*bucket{}
		burst   = float64(perMinute)
		refill  = burst / 60 // tokens per second
	)

	return func(c *gin.Context) {
		user := CurrentUser(c)
		if user == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"detail": "Not authenticated"})
			return
		}

		now := time.Now()
		mu.Lock()
		b, ok := buckets[user.ID]
		if !ok {
			pruneBuckets(buckets, now)
			b = &bucket{tokens: burst, last: now}
			buckets[user.ID] = b
		}
		b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*refill)
		b.last = now
		allowed := b.tokens >= 1
		if allowed {
			b.tokens--
		}
		mu.Unlock()

		if !allowed {
			rateLog.Warnf("Rate limit hit (user=%s path=%s limit=%d/min)",
				user.ID, c.FullPath(), perMinute)
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				gin.H{"detail": "Rate limit exceeded, retry in a minute"})
			return
		}
		c.Next()
	}
}

type bucket struct {
	tokens float64
	last   time.Time
}

// pruneBuckets drops long-idle entries so the map doesn't grow with every
// user who ever called the route. An idle bucket has refilled completely,
// so deleting it is equivalent to keeping it. Called under mu.
func pruneBuckets(buckets map[string]*bucket, now time.Time) {
	if len(buckets) < 1024 {
		return
	}
	for id, b := range buckets {
		if now.Sub(b.last) > time.Hour {
			delete(buckets, id)
		}
	}
}
