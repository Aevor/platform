package main

import (
	"log"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
	// Connect to PostgreSQL
	db, err := database.Connect()
	if err != nil {
		log.Fatal("failed to connect database: ", err)
	}

	// Run user migrations
	log.Println("running user migrations")

	err = users.Migrate(db)
	if err != nil {
		log.Fatal("failed to migrate users table: ", err)
	}

	log.Println("user migration completed")

	// Create Gin router
	router := gin.Default()

	// Health check endpoint
	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	log.Println("server running on :8080")

	err = router.Run(":8080")
	if err != nil {
		log.Fatal("failed to start server: ", err)
	}
}
