package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/discovery"
	"github.com/Aevor/platform/services/api/internal/extraction"
	"github.com/Aevor/platform/services/api/internal/filtering"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/indexing"
	"github.com/Aevor/platform/services/api/internal/repositories"
	"github.com/Aevor/platform/services/api/internal/representation"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/workspace"
	"github.com/Aevor/platform/services/api/pkg/config"
	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
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
		userService,
		jwtManager,
		ghClient,
		cfg.GitHubTokenEncryptionKey,
	)
	authHandler := auth.NewHandler(authService)

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
	)
	// Clone-URL policy from configuration (production default: https to
	// github.com only; file:// is a documented local-development opt-in).
	repositoriesService.ConfigureCloneURLPolicy(
		cfg.CloneAllowedHosts,
		cfg.CloneAllowFileTransport,
	)
	repositoriesHandler := repositories.NewHandler(repositoriesService)

	router := gin.New()
	router.Use(
		gin.LoggerWithConfig(gin.LoggerConfig{
			SkipQueryString: true,
		}),
		gin.Recovery(),
	)

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	router.GET(
		"/auth/github/login",
		authHandler.GitHubLogin,
	)

	router.GET(
		"/auth/github/callback",
		authHandler.GitHubCallback,
	)

	router.GET(
		"/users/me",
		auth.RequireAuth(jwtManager),
		authHandler.GetMe,
	)

	router.GET(
		"/repositories",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.List,
	)

	router.POST(
		"/repositories",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Select,
	)

	router.GET(
		"/repositories/selected",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.ListSelected,
	)

	router.DELETE(
		"/repositories/:id",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Delete,
	)

	router.POST(
		"/repositories/:id/issues/sync",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.SyncIssues,
	)

	router.POST(
		"/repositories/:id/pull-requests/sync",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.SyncPullRequests,
	)

	router.POST(
		"/repositories/:id/commits/sync",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.SyncCommits,
	)

	router.POST(
		"/repositories/:id/clone",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Clone,
	)

	router.POST(
		"/repositories/:id/discover",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Discover,
	)

	router.POST(
		"/repositories/:id/filter",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Filter,
	)

	router.POST(
		"/repositories/:id/extract",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Extract,
	)

	// Task 3e gap fix: the chunk route was never registered when chunking
	// was wired; it existed only in tests. Registered here alongside the
	// Task 3f representation route.
	router.POST(
		"/repositories/:id/chunk",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Chunk,
	)

	router.POST(
		"/repositories/:id/represent",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Represent,
	)

	// Task 3g: metadata-only index over represented chunks. Three
	// endpoints: rebuild, list files, and query.
	router.POST(
		"/repositories/:id/index",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.Index,
	)

	router.GET(
		"/repositories/:id/index/files",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.IndexedFiles,
	)

	router.POST(
		"/repositories/:id/index/lookup",
		auth.RequireAuth(jwtManager),
		repositoriesHandler.LookupIndexed,
	)

	log.Printf("server running on :%s", cfg.Port)

	err = router.Run(":" + cfg.Port)

	if err != nil {
		log.Fatal(err)
	}
}
