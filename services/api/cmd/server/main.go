package main

import (
	"log"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
	// Connect PostgreSQL
	db, err := database.Connect()

	if err != nil {
		log.Fatal("failed to connect database: ", err)
	}

	// Run migrations
	log.Println("running user migrations")

	err = users.Migrate(db)

	if err != nil {
		log.Fatal("failed to migrate users table: ", err)
	}

	log.Println("user migration completed")

	// Build dependencies
	userRepository := users.NewRepository(db)
	userService := users.NewService(userRepository)
	userHandler := users.NewHandler(userService)

	router := gin.Default()

	// Health endpoint
	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	// User endpoints
	router.POST("/users", userHandler.CreateUser)
	router.GET("/users/:id", userHandler.GetUserByID)

	log.Println("server running on :8080")

	err = router.Run(":8080")

	if err != nil {
		log.Fatal(err)
	}
}
