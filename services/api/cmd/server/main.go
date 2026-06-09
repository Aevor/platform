package main

import (
	"log"

	"github.com/gin-gonic/gin"

	"github.com/Aevor/platform/services/api/pkg/database"
)

func main() {
	_, err := database.Connect()

	if err != nil {
		log.Fatal("failed to connect database:", err)
	}

	router := gin.Default()

	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	log.Println("server running on :8080")

	err = router.Run(":8080")

	if err != nil {
		log.Fatal(err)
	}
}
