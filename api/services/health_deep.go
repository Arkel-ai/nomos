package services

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var healthDeepLog = u.NewLogger("services.health")

// deepCheckTimeout bounds every probe. A readiness endpoint that can hang is
// worse than one that reports "down".
const deepCheckTimeout = 3 * time.Second

// HealthDeep is GET /api/health?deep=1 — a real readiness signal.
//
// The shallow probe is deliberately trivial and stays green while Postgres,
// S3, the gateway or the runner is dead (docs/PLAN.md §2.5). That is fine for
// liveness and useless for readiness, so this one actually talks to each
// dependency.
//
// Postgres is REQUIRED: unconfigured or unreachable means 503. S3 and the LLM
// gateway follow the project's optional-integration contract — unconfigured is
// "skipped" and does not fail readiness, but configured-and-failing is "down"
// and does. Only shapes are reported: statuses and latencies, never a key,
// never a DSN, never an endpoint credential (§7.4).
func HealthDeep(c *gin.Context, cfg config.Config) (int, any) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), deepCheckTimeout)
	defer cancel()

	checks := map[string]types.Check{
		"postgres":    checkPostgres(ctx),
		"casbin":      checkCasbin(),
		"s3":          checkS3(ctx),
		"llm_gateway": checkGateway(ctx, cfg),
	}

	status := "ok"
	for _, ch := range checks {
		if ch.Status == "down" {
			status = "degraded"
			break
		}
	}

	body := types.DeepHealthResponse{
		Status: status,
		Checks: checks,
		Features: map[string]bool{
			"vision_ocr": cfg.VisionOCREnabled,
		},
		Versions: map[string]string{
			"embedding_model": cfg.EmbeddingModel,
			"rerank_model":    cfg.RerankModel,
		},
	}

	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
		healthDeepLog.Warnf("Deep health degraded (postgres=%s casbin=%s s3=%s gateway=%s)",
			checks["postgres"].Status, checks["casbin"].Status,
			checks["s3"].Status, checks["llm_gateway"].Status)
	}
	return code, body
}

func checkPostgres(ctx context.Context) types.Check {
	start := time.Now()
	if u.DB == nil {
		return types.Check{Status: "down", Detail: "DATABASE_URL not configured"}
	}
	sqlDB, err := u.DB.DB()
	if err != nil {
		return types.Check{Status: "down", Detail: "pool unavailable", LatencyMS: ms(start)}
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return types.Check{Status: "down", Detail: "ping failed", LatencyMS: ms(start)}
	}
	return types.Check{Status: "ok", LatencyMS: ms(start)}
}

func checkCasbin() types.Check {
	if u.Enforcer == nil {
		return types.Check{Status: "down", Detail: "enforcer not initialized"}
	}
	return types.Check{Status: "ok"}
}

func checkS3(ctx context.Context) types.Check {
	start := time.Now()
	if u.S3 == nil {
		return types.Check{Status: "skipped", Detail: "not configured"}
	}
	if _, err := u.S3.BucketExists(ctx, u.S3Bucket); err != nil {
		return types.Check{Status: "down", Detail: "bucket unreachable", LatencyMS: ms(start)}
	}
	return types.Check{Status: "ok", LatencyMS: ms(start)}
}

// checkGateway probes the OpenAI-compatible gateway's model listing. It uses
// an explicit http.Client with a timeout — never http.DefaultClient — and the
// key travels only in the Authorization header of its own destination, never
// into a log line or the response body.
func checkGateway(ctx context.Context, cfg config.Config) types.Check {
	start := time.Now()
	if cfg.OpenRouterAPIKey == "" {
		return types.Check{Status: "skipped", Detail: "not configured"}
	}
	url := strings.TrimRight(cfg.OpenRouterBaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return types.Check{Status: "down", Detail: "bad base url", LatencyMS: ms(start)}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)

	client := &http.Client{Timeout: deepCheckTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return types.Check{Status: "down", Detail: "unreachable", LatencyMS: ms(start)}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return types.Check{Status: "down", Detail: http.StatusText(resp.StatusCode), LatencyMS: ms(start)}
	}
	return types.Check{Status: "ok", LatencyMS: ms(start)}
}

func ms(start time.Time) int64 { return time.Since(start).Milliseconds() }
