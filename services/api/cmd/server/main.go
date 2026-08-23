package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/repositories"
	"github.com/Aevor/platform/services/api/internal/users"
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
	ghClient := github.NewClient(&http.Client{Timeout: 10 * time.Second})
	jwtManager := auth.NewJWTManager(cfg.JWTSecret)

	authService := auth.NewService(
		oauthConfig,
		userService,
		jwtManager,
		ghClient,
		cfg.GitHubTokenEncryptionKey,
	)
	authHandler := auth.NewHandler(authService)

	repositoriesService := repositories.NewService(
		userService,
		ghClient,
		repositories.NewStore(db),
		cfg.GitHubTokenEncryptionKey,
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

	log.Printf("server running on :%s", cfg.Port)

	err = router.Run(":" + cfg.Port)

	if err != nil {
		log.Fatal(err)
	}
}
