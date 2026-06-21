package main

import (
	"log"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
	db, err := database.Connect()

	if err != nil {
		log.Fatal("failed to connect database: ", err)
	}

	log.Println("running user migrations")

	err = users.Migrate(db)

	if err != nil {
		log.Fatal("failed to migrate users table: ", err)
	}

	log.Println("user migration completed")

	userRepository := users.NewRepository(db)
	userService := users.NewService(userRepository)
	userHandler := users.NewHandler(userService)

	oauthConfig := auth.NewGitHubOAuthConfig()
	authService := auth.NewService(
		oauthConfig,
	)
	authHandler := auth.NewHandler(
		authService,
	)

	router := gin.Default()

	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	router.POST("/users", userHandler.CreateUser)

	router.GET("/users/:id", userHandler.GetUserByID)

	router.GET(
		"/users/github/:id",
		userHandler.GetUserByGitHubID,
	)

	router.GET(
		"/auth/github/login",
		authHandler.GitHubLogin,
	)

	log.Println("server running on :8080")

	err = router.Run(":8080")

	if err != nil {
		log.Fatal(err)
	}
}
