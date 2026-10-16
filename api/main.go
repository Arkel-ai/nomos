package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/controllers"
	"github.com/Arkel-ai/nomos/api/middlewares"
	"github.com/Arkel-ai/nomos/api/runner"
	"github.com/Arkel-ai/nomos/api/services"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var log = u.NewLogger("app.main")

func main() {
	_ = godotenv.Load()

	cfg := config.Load()
	u.InitLogger(cfg.LogLevel)
	u.InitClients(cfg)

	// Invitation feature: seed the role policy matrix, wire boot-time config
	// (accept-link base URL, bootstrap admin email), and ensure the first
	// admin holds the admin role once its Better Auth account exists.
	if err := u.SeedRolePolicies(); err != nil && !errors.Is(err, u.ErrAuthNotConfigured) {
		log.Warnf("Seed role policies failed: %v", err)
	}
	services.ConfigureInvitations(cfg)
	services.BootstrapAdmin()

	// Background work lives in goroutines inside this process: the allowed
	// architecture is one pod with two containers, so there is no worker
	// Deployment, no sidecar and no CronJob, and the queue is Postgres
	// (docs/PLAN.md §2). bgCtx is cancelled BEFORE srv.Shutdown so the runner
	// stops claiming new steps while in-flight requests are still draining.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	runner.Start(bgCtx, cfg)

	// Bring up the public corpus if the database does not already have it, so
	// a fresh cluster is usable without a manual admin call. In its own
	// goroutine: the load takes ~45s and must never delay the listener or the
	// readiness probe. Skips cleanly when already loaded, when another replica
	// holds the claim, or when the source has no credentials.
	go services.SeedCorpus(bgCtx, cfg)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middlewares.RequestLog())
	r.Use(cors.New(corsConfigFrom(cfg)))

	api := r.Group("/api")
	// Liveness, plus a real readiness mode at ?deep=1 that actually checks
	// Postgres, S3 and the LLM gateway (§2.5). The trivial probe alone stays
	// green while every dependency is dead, which is exactly the failure the
	// platform must be able to see.
	api.GET("/health", controllers.HealthProbe(cfg))
	// The current user. Requires a valid Better Auth session, supplied either
	// as the `better-auth.session_token` cookie (same-origin SPA) or
	// `Authorization: Bearer <token>`. Validation is a Postgres session lookup
	// — see middlewares/auth.go.
	api.GET("/me", middlewares.RequireAuth(), controllers.Me)

	// User management (admin panel): list users with their roles, and
	// change a user's role. Both gated by the "admin" object.
	api.GET("/admin/users",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "read"),
		controllers.ListUsers)
	api.PUT("/admin/users/:id/role",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "write"),
		controllers.SetUserRole)

	// Invitation flow. Create/list/revoke are admin-only; validate is
	// public (the token is the secret, used by the accept page before the
	// user has an account); accept only needs a session (the just-created
	// user redeeming their token).
	api.POST("/invitations",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "write"),
		controllers.CreateInvitation)
	api.GET("/invitations",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "read"),
		controllers.ListInvitations)
	api.DELETE("/invitations/:id",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "write"),
		controllers.RevokeInvitation)
	api.GET("/invitations/validate", controllers.ValidateInvitation)
	api.POST("/invitations/accept", middlewares.RequireAuth(), controllers.AcceptInvitation)

	// -------------------------------------------------------------------
	// NOMOS (docs/PLAN.md §10)
	//
	// Every route below requires a session AND resolves the caller's firm.
	// Dossier / piece / run / artifact routes then authorize with
	// RequireTenantPermission, whose Casbin DOMAIN is the FIRM ID — never the
	// starter's "*", which would erase tenancy and, because SeedTenantPolicies
	// refuses to write into "*", would also 403 every real user (§6.1).
	// The admin corpus routes are the deliberate exception: the public corpus
	// belongs to no tenant, so they stay on the global admin role.
	// Per-dossier sharing is the Casbin object `dossier:<id>`.
	// -------------------------------------------------------------------

	// Conversations — the product's primary entry point. Every turn is one
	// agent run, shown in the thread (§1, §11); there is no separate run page.
	// Creating a thread with an inline first message and answering a parked
	// question both release paid agent work, so they carry the per-user cap.
	api.POST("/conversations",
		middlewares.RequireAuth(),
		middlewares.RequireTenant(),
		middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
		controllers.CreateConversation)
	api.GET("/conversations", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.ListConversations)
	api.GET("/conversations/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.GetConversation)
	api.PATCH("/conversations/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.UpdateConversation)
	api.DELETE("/conversations/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.DeleteConversation)
	// Posting a turn is what spends: it starts an agent run of many model
	// calls, so it carries the same per-user cap as the reference LLM routes. Reading the thread back
	// (the two transports below) touches no paid API and is not capped —
	// a severed stream must be free to reconnect.
	api.POST("/conversations/:id/messages",
		middlewares.RequireAuth(),
		middlewares.RequireTenant(),
		middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
		controllers.PostMessage)
	// Two transports over ONE durable log (§4.5b): SSE replays from the
	// cursor and then goes live; the poll reads the same rows. A severed
	// stream costs the view, never the work.
	api.GET("/conversations/:id/events", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.ConversationEvents)
	api.GET("/conversations/:id/stream", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.ConversationStream)

	// Dossiers and pieces — slice 1.
	api.POST("/dossiers",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		controllers.CreateDossier)
	api.GET("/dossiers",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.ListDossiers)
	api.GET("/dossiers/:id",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.GetDossier)
	api.DELETE("/dossiers/:id",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		controllers.DeleteDossier)
	// Ingestion pays for OCR, qualification and embedding on every piece.
	api.POST("/dossiers/:id/pieces",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
		controllers.IngestPieces)
	api.GET("/dossiers/:id/pieces",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.ListPieces)
	api.PATCH("/pieces/:id",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		controllers.UpdatePiece)
	api.GET("/pieces/:id/download",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.DownloadPiece)
	api.POST("/dossiers/:id/export",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		controllers.ExportDossier)

	// Runs — read, answer and cancel the runs a turn started. These endpoints
	// only read and write rows; the work happens in the runner goroutines
	// started above.
	api.GET("/runs/:id",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.GetRun)
	api.GET("/runs/:id/events",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.RunEvents)
	api.POST("/runs/:id/answers",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
		controllers.AnswerRun)
	api.POST("/runs/:id/cancel",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionWrite),
		controllers.CancelRun)
	api.GET("/runs/:id/artifacts",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.RunArtifacts)
	api.GET("/artifacts/:id/download",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.DownloadArtifact)

	// Agent citations and coverage — read-only,
	// authorized through the run; a foreign id is a non-disclosing 404.
	api.GET("/runs/:id/citations",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.RunCitations)
	api.GET("/citations/:id",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.GetCitation)
	api.GET("/runs/:id/coverage",
		middlewares.RequireAuth(),
		middlewares.RequireTenantPermission(types.ObjectDossier, types.ActionRead),
		controllers.RunCoverage)

	// Retrieval, compétences and memory — slice 2.
	// Retrieval embeds the query and reranks the candidates — both billed.
	api.POST("/search",
		middlewares.RequireAuth(),
		middlewares.RequireTenant(),
		middlewares.RateLimit(cfg.LLMRateLimitPerMinute),
		controllers.Search)
	api.GET("/skills", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.ListSkills)
	api.POST("/skills", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.CreateSkill)
	api.GET("/skills/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.GetSkill)
	api.DELETE("/skills/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.DeleteSkill)
	api.GET("/memory", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.ListMemory)
	api.POST("/memory", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.UpsertMemory)
	api.DELETE("/memory/:id", middlewares.RequireAuth(), middlewares.RequireTenant(), controllers.DeleteMemory)

	// Public-corpus administration. Loading is an admin action, never
	// deployment behaviour (§9.1(10)).
	api.POST("/admin/corpus/sync",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "write"),
		controllers.SyncCorpus)
	api.GET("/admin/corpus/status",
		middlewares.RequireAuth(),
		middlewares.RequirePermission("admin", "write"),
		controllers.CorpusStatus)

	// The LOCAL blob backend has no object store to presign against, so it
	// mints an app-relative signed URL and the API serves the bytes. That is a
	// development-only path: with S3 configured, downloads are real presigned
	// URLs and this route is not registered at all.
	if u.Blob.Backend() == "local" {
		api.GET("/blobs/*key", controllers.ServeBlob)
		log.Warn("Blob backend is the local filesystem — serving GET /api/blobs/*key. Development only.")
	}

	srv := &http.Server{
		Addr:    ":8000",
		Handler: r,
	}

	go func() {
		log.Infof("Starting Nomos API (log_level=%s)", cfg.LogLevel)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// Stop the background workers FIRST. The pod can die mid-mission and a
	// drafting step takes far longer than the 10 s graceful window, so the
	// goal here is not to finish the work — leases and idempotent
	// publication are what recover it (§2.1, §4.2). The goal is to stop
	// CLAIMING new steps before the HTTP server drains, so nothing is left
	// leased by a process that is about to disappear.
	log.Info("Shutting down Nomos API")
	stopBackground()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Errorf("Graceful shutdown failed: %v", err)
	}
}

func corsConfigFrom(cfg config.Config) cors.Config {
	c := cors.DefaultConfig()
	c.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	c.AllowHeaders = []string{"Origin", "Content-Length", "Content-Type", "Authorization", "X-Request-ID"}
	c.ExposeHeaders = []string{"X-Request-ID"}
	c.AllowCredentials = true

	if len(cfg.CorsOrigins) == 1 && cfg.CorsOrigins[0] == "*" {
		c.AllowAllOrigins = true
	} else {
		c.AllowOrigins = cfg.CorsOrigins
	}
	return c
}
