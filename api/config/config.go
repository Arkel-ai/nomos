package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	LogLevel    string
	CorsOrigins []string
	DatabaseURL string

	// AppName is the app identifier, injected as APP_NAME by the deployment
	// and used as the per-app S3 key prefix.
	AppName string

	// AppBaseURL is the public origin of the app, used to build invitation
	// accept links (e.g. https://app.example.com/accept-invite?token=...).
	// Defaults to the dev front origin.
	AppBaseURL string

	// BootstrapAdminEmail is the email of the first admin. On boot the Go
	// API ensures this user (once Better Auth has created the account) holds
	// the "admin" Casbin role; the Better Auth account itself (with its
	// password) is created by the front Node server. Empty disables it.
	BootstrapAdminEmail string

	// S3-compatible object storage. When the endpoint, bucket or access/secret
	// keys are empty the S3 client stays nil and the file endpoints return
	// 503 — same "optional integration" contract as OpenAI / Postgres.
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string

	// LLMRateLimitPerMinute caps how many LLM-backed requests a single
	// authenticated user may make per minute. Every route that spends money
	// downstream sits behind it (see main.go). In-memory, so the cap is per
	// API pod.
	LLMRateLimitPerMinute int

	// ---------------------------------------------------------------
	// Nomos — LLM gateway (OpenRouter, OpenAI-compatible).
	//
	// A deployment can point legal inference at a different (or self-hosted,
	// see docs/PLAN.md §13) endpoint. Empty key → the Nomos LLM services
	// return 503, the same optional-integration contract as everything else.
	// ---------------------------------------------------------------
	OpenRouterAPIKey  string
	OpenRouterBaseURL string

	// Model routing, per docs/PLAN.md §12. PlannerModel selects and
	// parameterises a code-owned workflow (§4.1); DrafterModel writes
	// grounded prose (slice 2); ExtractionModel qualifies pieces against a
	// server-owned JSON schema; VisionModel reads scanned pages.
	PlannerModel    string
	ExtractionModel string
	DrafterModel    string
	VisionModel     string
	// WebSearchModel is the :online-suffixed model the web research loop
	// calls. The suffix is what enables OpenRouter's web plugin; without it
	// the step is an ordinary completion answering from memory, which is the
	// one thing this loop must never do.
	WebSearchModel string

	// Retrieval. EmbeddingDims is a real commitment: it is baked into the
	// halfvec(N) column type, so changing it means a new column plus a
	// dual-read reindex, never a config flip (§9.1).
	EmbeddingModel string
	EmbeddingDims  int
	RerankModel    string

	// VisionOCREnabled gates remote vision OCR. Default OFF: local
	// tesseract is the default processing mode, and remote vision is an
	// administrator-enabled mode with an allowlisted route (§7.2).
	VisionOCREnabled bool

	// PISTE (Légifrance + Judilibre) OAuth2 client credentials. Empty
	// disables the connectors; the corpus loader then reports 503.
	PisteClientID     string
	PisteClientSecret string

	// PISTE endpoints. Sandbox and production are SEPARATE environments with
	// separate credentials — a sandbox token cannot authenticate a production
	// request — so the hosts have to move together with the id/secret pair
	// rather than being compiled in. Empty values fall back to production.
	PisteTokenURL     string
	LegifranceBaseURL string
	JudilibreBaseURL  string

	// CorpusAutoseedSources are the corpus sources loaded at boot when they
	// are not already loaded, so a fresh cluster comes up with a usable
	// corpus. Empty disables it entirely. See services.SeedCorpus for why
	// this is a bounded exception to "loading is an admin action".
	CorpusAutoseedSources []string

	// Differentiated runner pools (§4.2). A single goroutine pool would let
	// a CPU-bound OCR step starve cheap DB transitions, and let a 3-minute
	// LLM call queue ahead of them. Per pod, not per cluster.
	StepConcurrencyOCR    int
	StepConcurrencyRender int
	StepConcurrencyLLM    int
	StepConcurrencyDB     int
}

func Load() Config {
	return Config{
		LogLevel:    getenv("LOG_LEVEL", "INFO"),
		CorsOrigins: splitCSV(getenv("CORS_ORIGINS", "http://localhost:5173,http://localhost:3000")),
		// DATABASE_URL is the Postgres DSN. When empty, the DB / auth /
		// authorization endpoints return 503 (server not configured) — the
		// same "optional integration" contract used for OpenAI.
		DatabaseURL: os.Getenv("DATABASE_URL"),

		AppName: os.Getenv("APP_NAME"),

		AppBaseURL:          getenv("APP_BASE_URL", "http://localhost:5173"),
		BootstrapAdminEmail: os.Getenv("BOOTSTRAP_ADMIN_EMAIL"),

		S3Endpoint:  os.Getenv("S3_ENDPOINT"),
		S3Region:    os.Getenv("S3_REGION"),
		S3Bucket:    os.Getenv("S3_BUCKET"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),

		LLMRateLimitPerMinute: parsePositiveInt(os.Getenv("LLM_RATE_LIMIT_PER_MINUTE"), 20),

		// Nomos — LLM gateway. The key falls back to OPENAI_API_KEY, which
		// some deployments set instead.
		OpenRouterAPIKey:  getenv("OPENROUTER_API_KEY", os.Getenv("OPENAI_API_KEY")),
		OpenRouterBaseURL: getenv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),

		PlannerModel:    getenv("PLANNER_MODEL", "google/gemini-3.7-flash"),
		ExtractionModel: getenv("EXTRACTION_MODEL", "google/gemini-3.7-flash"),
		DrafterModel:    getenv("DRAFTER_MODEL", "google/gemini-3.7-flash"),
		VisionModel:     getenv("VISION_MODEL", "anthropic/claude-haiku-5.5"),
		WebSearchModel:  getenv("WEB_SEARCH_MODEL", "google/gemini-3.7-flash:online"),

		EmbeddingModel: getenv("EMBEDDING_MODEL", "openai/text-embedding-3-small"),
		EmbeddingDims:  getenvInt("EMBEDDING_DIMS", 1536),
		RerankModel:    getenv("RERANK_MODEL", "cohere/rerank-4-fast"),

		VisionOCREnabled: parseBool(os.Getenv("VISION_OCR_ENABLED")),

		PisteClientID:     os.Getenv("PISTE_CLIENT_ID"),
		PisteClientSecret: os.Getenv("PISTE_CLIENT_SECRET"),
		PisteTokenURL:     os.Getenv("PISTE_TOKEN_URL"),
		LegifranceBaseURL: os.Getenv("LEGIFRANCE_BASE_URL"),
		JudilibreBaseURL:  os.Getenv("JUDILIBRE_BASE_URL"),

		CorpusAutoseedSources: splitList(getenv("CORPUS_AUTOSEED_SOURCES", "legifrance")),

		StepConcurrencyOCR:    getenvInt("STEP_CONCURRENCY_OCR", 1),
		StepConcurrencyRender: getenvInt("STEP_CONCURRENCY_RENDER", 1),
		StepConcurrencyLLM:    getenvInt("STEP_CONCURRENCY_LLM", 4),
		StepConcurrencyDB:     getenvInt("STEP_CONCURRENCY_DB", 8),
	}
}

// parsePositiveInt reads a positive integer env var. Unset, unparseable,
// zero, and negative all fall back to def — a rate limit of 0 would
// silently disable the cap instead of tightening it.
func parsePositiveInt(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// parseBool reports whether an env var is set to a truthy value. Empty,
// unset, or anything other than the accepted truthy strings is false.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// getenvInt reads a positive integer env var, falling back to def when the
// variable is unset, empty or not a positive number. A zero pool size would
// silently stall the runner, so non-positive values are rejected.
func getenvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// splitList parses a comma-separated env var into trimmed, non-empty entries.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
