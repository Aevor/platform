package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/bodylimit"
	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/cors"
	"github.com/Aevor/platform/services/api/internal/diagnostics"
	"github.com/Aevor/platform/services/api/internal/discovery"
	"github.com/Aevor/platform/services/api/internal/extraction"
	"github.com/Aevor/platform/services/api/internal/filtering"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/indexing"
	"github.com/Aevor/platform/services/api/internal/jobs"
	"github.com/Aevor/platform/services/api/internal/ratelimit"
	"github.com/Aevor/platform/services/api/internal/repositories"
	"github.com/Aevor/platform/services/api/internal/representation"
	"github.com/Aevor/platform/services/api/internal/requestid"
	"github.com/Aevor/platform/services/api/internal/securityheaders"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/webhook"
	"github.com/Aevor/platform/services/api/internal/workspace"
	"github.com/Aevor/platform/services/api/pkg/config"
	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
	// A single cancellable context bounds the server AND the job worker so
	// SIGINT/SIGTERM shuts both down together.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()

	if err != nil {
		log.Fatal("invalid configuration: ", err)
	}

	db, err := database.Connect(cfg)

	if err != nil {
		log.Fatal("failed to connect database: ", err)
	}

	log.Println("running user migrations")
	err = users.Migrate(db)
	if err != nil {
		log.Fatal("failed to migrate users table: ", err)
	}
	log.Println("user migration completed")
	log.Println("running selected-repository migrations")
	err = repositories.Migrate(db)
	if err != nil {
		log.Fatal("failed to migrate selected_repositories table: ", err)
	}

	log.Println("selected-repository migration completed")
	log.Println("running webhook target migrations")
	err = webhook.Migrate(db)
	if err != nil {
		log.Fatal("failed to migrate webhook_targets table: ", err)
	}
	log.Println("webhook target migration completed")

	userRepository := users.NewRepository(db)
	userService := users.NewService(userRepository)

	oauthConfig := auth.NewGitHubOAuthConfig(cfg)
	ghClient := github.NewClient(
		&http.Client{Timeout: 10 * time.Second},
		github.WithBaseURL(cfg.GitHubBaseURL),
	)
	jwtManager := auth.NewJWTManager(cfg.JWTSecret)

	authService := auth.NewService(
		oauthConfig,
		ghClient,
		userService,
		cfg.JWTSecret,
		cfg.GitHubTokenEncryptionKey,
	)
	// The OAuth state cookie carries the PKCE verifier: Secure is mandatory
	// behind https and only defaults off for local http development.
	authHandler := auth.NewHandler(authService, cfg.FrontendURL).WithCookieSecure(cfg.OAuthCookieSecure)

	// The controlled workspace root is REQUIRED and validated at startup:
	// repository workspaces are never written to arbitrary locations.
	workspaces, err := workspace.NewManager(cfg.WorkspaceRoot)

	if err != nil {
		log.Fatalf("workspace root configuration: %v", err)
	}

	// One shared filtering configuration feeds both the filter endpoint and
	// content extraction, so selection budgets and read caps never diverge.
	filteringService := filtering.NewService(filtering.Options{
		MaxFileSize:      cfg.FilterMaxFileSize,
		MaxTotalBytes:    cfg.FilterMaxTotalBytes,
		MaxSelectedFiles: cfg.FilterMaxFiles,
	})

	// The AI analysis service client communicates with the separate AI
	// repository over HTTP. nil apiKey means no Authorization header is sent.
	aiClient := ai.NewClient(
		&http.Client{Timeout: 30 * time.Second},
		ai.WithBaseURL(cfg.AIServiceURL),
		ai.WithAPIKey(cfg.AIServiceAPIKey),
	)

	repositoriesService := repositories.NewService(
		userService,
		ghClient,
		repositories.NewStore(db),
		cfg.GitHubTokenEncryptionKey,
		workspaces,
		workspace.NewGoGitCloner().WithDepth(1),
		discovery.NewService(discovery.Options{}),
		filteringService,
		extraction.NewService(filteringService, extraction.Options{
			MaxFileSize: cfg.FilterMaxFileSize,
		}),
		chunking.NewService(chunking.Options{}),
		representation.NewService(),
		indexing.New(indexing.Options{}),
		aiClient,
		cfg.GitHubWebhookSecret,
		nil, // job service wired immediately below (job definitions close over this service)
	)

	// The job registry closes over the service, and the service enqueues jobs,
	// so the two are wired in that order: definitions first, then the service.
	jobStore := jobs.NewMemoryStore()
	jobRegistry := jobs.NewRegistry()
	repositories.RegisterJobDefinitions(jobRegistry, repositoriesService)

	jobService, err := jobs.NewService(jobStore, jobs.Options{
		Registry: jobRegistry,
		Recorder: repositoriesService,
	})
	if err != nil {
		log.Fatal("failed to create job service: ", err)
	}

	repositoriesService.SetJobsService(jobService)

	// Clone-URL policy from configuration (production default: https to
	// github.com only; file:// is a documented local-development opt-in).
	repositoriesService.ConfigureCloneURLPolicy(
		cfg.CloneAllowedHosts,
		cfg.CloneAllowFileTransport,
	)
	repositoriesHandler := repositories.NewHandlerWithJobs(repositoriesService, jobService)

	// Set DB on webhook store so it can persist targets.
	repositoriesService.GetWebhookStore().SetDB(db)

	// Start job worker in background.
	jobWorker := jobs.NewWorker(jobService, "api-worker", jobs.WorkerOptions{})
	go jobWorker.Start()
	defer jobWorker.Shutdown(ctx)

	// Effective limits are resolved from configuration ONCE, here, so the rest
	// of the process and the startup diagnostics all report the same values
	// instead of each re-deriving (and possibly disagreeing about) them.
	// Report every resolved limit and every dependency finding at startup, so a
	// first run explains its own configuration instead of failing later with an
	// opaque connection error. A blocking finding exits before the port opens.
	report := diagnostics.Collect(ctx, cfg, db, workspaces, aiClient)
	if report.Worst() == diagnostics.SeverityError {
		log.Fatal("startup aborted: resolve the [ERROR] diagnostics above (run: go run ./cmd/doctor)")
	}

	maxRequestBody := report.Limits.MaxRequestBody
	corsOrigins, corsCredentials := cfg.CORSParams()
	rateLimitEnabled, rateRPM, rateRPH, rateBurst := cfg.RateLimitParams()

	// Two rate limiters, keyed differently on purpose:
	//  - public routes have no verified identity yet, so they are limited per
	//    client IP (login, callback, webhooks);
	//  - authenticated routes are limited per verified user, so one user
	//    cannot exhaust the budget of everyone behind the same NAT/egress.
	var publicRateLimit, userRateLimit gin.HandlerFunc

	if rateLimitEnabled {
		publicRateLimit = ratelimit.NewRateLimitMiddleware(
			ratelimit.NewMemoryLimiter(time.Minute),
			ratelimit.DefaultIPKey, rateRPM, rateRPH, rateBurst,
		).Handler()
		userRateLimit = ratelimit.NewRateLimitMiddleware(
			ratelimit.NewMemoryLimiter(time.Minute),
			ratelimit.UserIDKey, rateRPM, rateRPH, rateBurst,
		).Handler()
	}

	bodyLimitCfg := bodylimit.DefaultBodyLimitConfig()
	bodyLimitCfg.MaxBytes = maxRequestBody
	// The webhook payload cap is deliberately much tighter than the general
	// request cap: a GitHub event is small, and an oversized one is either a
	// mistake or an attempt to exhaust memory before signature verification.
	bodyLimitCfg.ExemptPaths = nil

	corsCfg := cors.DefaultCORSConfig()
	corsCfg.AllowedOrigins = corsOrigins
	corsCfg.AllowCredentials = corsCredentials

	securityCfg := securityheaders.DefaultSecurityHeadersConfig()

	router := gin.New()
	router.Use(
		// Correlation id first: every later log line, including the panic
		// recovery line, can then be tied back to the caller's request.
		requestid.Middleware(),
		gin.LoggerWithConfig(gin.LoggerConfig{
			// The OAuth callback carries `code` and `state` in the query
			// string. Logging it would put a single-use credential and the
			// CSRF state in the access log, so the query string is dropped.
			SkipQueryString: true,
		}),
		gin.Recovery(),
		securityheaders.SecurityHeadersMiddleware(securityCfg),
		cors.CORSMiddleware(corsCfg),
		bodylimit.BodyLimitMiddleware(bodyLimitCfg),
	)

	// Liveness: the process is up and the HTTP stack is serving. It
	// deliberately touches NO dependency, so a database blip cannot cause an
	// orchestrator to kill an otherwise healthy process.
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	// Readiness: this instance can actually serve traffic, which means the
	// database answers AND the schema it needs is present. A container that is
	// live but not ready is removed from the load balancer instead of serving
	// 500s to users.
	readyHandler := func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"reason": "database_unavailable",
			})
			return
		}

		pingCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()

		if err := sqlDB.PingContext(pingCtx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"reason": "database_unreachable",
			})
			return
		}

		// The same required-table list doctor and `migrate verify` use, so the
		// three cannot disagree about whether this instance is usable.
		if missing := diagnostics.MissingTables(pingCtx, db); len(missing) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"reason": "schema_incomplete",
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
		})
	}

	router.GET("/health/ready", readyHandler)

	router.GET(
		"/auth/github/login",
		publicRateLimit,
		authHandler.GitHubLogin,
	)

	router.GET(
		"/auth/github/callback",
		publicRateLimit,
		authHandler.GitHubCallback,
	)

	router.GET(
		"/users/me",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		authHandler.Me,
	)

	router.GET(
		"/contribution-summary",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetContributionSummary,
	)

	router.GET(
		"/skills",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetSkills,
	)

	router.GET(
		"/recommendations",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetRecommendations,
	)

	router.GET(
		"/repositories",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.List,
	)

	router.POST(
		"/repositories",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Select,
	)

	router.GET(
		"/repositories/selected",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListSelected,
	)

	router.DELETE(
		"/repositories/:id",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Delete,
	)

	router.POST(
		"/repositories/:id/issues/sync",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.SyncIssues,
	)

	router.POST(
		"/repositories/:id/pull-requests/sync",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.SyncPullRequests,
	)

	router.POST(
		"/repositories/:id/commits/sync",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.SyncCommits,
	)

	router.POST(
		"/repositories/:id/clone",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Clone,
	)

	router.POST(
		"/repositories/:id/discover",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Discover,
	)

	router.POST(
		"/repositories/:id/filter",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Filter,
	)

	router.POST(
		"/repositories/:id/extract",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Extract,
	)

	// Task 3e gap fix: the chunk route was never registered when chunking
	// was wired; it existed only in tests. Registered here alongside the
	// Task 3f representation route.
	router.POST(
		"/repositories/:id/chunk",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Chunk,
	)

	router.POST(
		"/repositories/:id/represent",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Represent,
	)

	// Task 3g: metadata-only index over represented chunks. Three
	// endpoints: rebuild, list files, and query.
	// router.POST(
	// 	"/repositories/:id/index",

	// router.POST(
	// 	"/repositories/:id/prepare",
	// 	auth.RequireAuth(jwtManager),
	// 	repositoriesHandler.Prepare,
	// )
	// 	auth.RequireAuth(jwtManager),
	// 	repositoriesHandler.Index,
	// )

	// Task 3g: metadata-only index over represented chunks. Three
	// endpoints: rebuild, list files, and query.
	router.POST(
		"/repositories/:id/index",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Index,
	)

	router.POST(
		"/repositories/:id/prepare",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Prepare,
	)
	router.GET(
		"/repositories/:id/index/files",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.IndexedFiles,
	)

	router.POST(
		"/repositories/:id/index/lookup",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.LookupIndexed,
	)

	// Task 3h: AI analysis over indexed repository context.
	router.POST(
		"/repositories/:id/analyze",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.Analyze,
	)

	// Task 3h read-back: durable, ownership-gated codebase analysis history.
	router.GET(
		"/repositories/:id/analyses",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListAnalyses,
	)

	// Repository Impact Analysis: what may be affected by a proposed target
	// (file/symbol) or a generated change set. Deterministic direct/possible
	// impact, optionally enriched with grounded AI semantic findings.
	router.POST(
		"/repositories/:id/impact-analysis",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.AnalyzeImpact,
	)

	// Issue solution pipeline: structured issue analysis, solution proposal,
	// and generate-changes (reviewable diff). These produce reviewable output
	// only — never commits, pushes, or PRs.
	router.POST(
		"/repositories/:id/issues/:issueID/analyze",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.AnalyzeIssue,
	)

	router.POST(
		"/repositories/:id/issues/:issueID/propose",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ProposeSolution,
	)

	router.POST(
		"/repositories/:id/issues/:issueID/generate-changes",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GenerateChanges,
	)

	// Change-set workflow: explicit user approval, controlled apply of the
	// exact approved change set into the local workspace, and structured
	// validation of the applied result. Apply never commits or pushes.
	router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/approve",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ApproveChangeSet,
	)

	router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/apply",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ApplyChangeSet,
	)

	router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/validate",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ValidateChangeSet,
	)

	// Delivery: publish a validated change set as branch → commit → push →
	// pull request. Idempotent and never destructive: a divergent remote
	// branch is refused, an existing open PR for head+base is reused.
	router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/publish",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.PublishChangeSet,
	)

	// PR details: the live GitHub view of one pull request (record, CI
	// checks, reviews, comments, changed files) for the owned repository.
	router.GET(
		"/repositories/:id/pull-requests/:number",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetPullRequestDetails,
	)

	// PR feedback analysis: a live, grounded AI analysis of one pull
	// request's feedback (GitHub facts + PR-scoped bounded context), with
	// GitHub's source of truth and the AI interpretation kept separate.
	router.POST(
		"/repositories/:id/pull-requests/:number/analyze-feedback",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.AnalyzePullRequestFeedback,
	)

	// Validation plan: a read-only preview of the controlled checks that would
	// run for a change set, so the owner sees them BEFORE anything executes.
	router.GET(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/validation-plan",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ValidationPlan,
	)

	// Repository intelligence: the deterministic profile (optionally enriched
	// by AI) and its revision-bound staleness. Refresh is ASYNC: it clones and
	// re-profiles the repository, so it is enqueued as a job rather than run
	// inside the request.
	router.GET(
		"/repositories/:id/intelligence",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetIntelligence,
	)

	router.POST(
		"/repositories/:id/intelligence/refresh",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.RefreshIntelligence,
	)

	// Engineering runs: the durable, inspectable trace of one issue attempt,
	// and the activity feed the dashboard reads.
	router.GET(
		"/repositories/:id/runs",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListRuns,
	)

	router.GET(
		"/repositories/:id/activity",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListRepositoryActivity,
	)

	router.GET(
		"/repositories/:id/runs/:runID",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetRun,
	)

	// The run's audit trail: every meaningful stage transition, with coarse
	// error categories only (never raw error text).
	router.GET(
		"/repositories/:id/runs/:runID/events",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListRunEvents,
	)

	// Background jobs. Every job is ownership-scoped to the caller, so a job
	// belonging to another user is indistinguishable from one that does not
	// exist. Mutations are explicit owner actions (cancel/retry).
	router.GET(
		"/repositories/:id/jobs",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListJobs,
	)

	router.GET(
		"/repositories/:id/jobs/:jobID",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.GetJob,
	)

	router.GET(
		"/repositories/:id/jobs/:jobID/events",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.ListJobEvents,
	)

	router.POST(
		"/repositories/:id/jobs/:jobID/cancel",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.CancelJob,
	)

	router.POST(
		"/repositories/:id/jobs/:jobID/retry",
		auth.RequireAuth(jwtManager),
		userRateLimit,
		repositoriesHandler.RetryJob,
	)

	// GitHub webhooks are NOT authenticated with an Aevor JWT: GitHub cannot
	// present one. The signature IS the credential, so this route is gated on
	// HMAC verification and rate limited, never on RequireAuth.
	router.POST(
		"/webhooks/github",
		publicRateLimit,
		repositoriesHandler.HandleGitHubWebhook,
	)

	log.Printf("server running on :%s", cfg.Port)

	err = router.Run(":" + cfg.Port)

	if err != nil {
		log.Fatal(err)
	}
}
