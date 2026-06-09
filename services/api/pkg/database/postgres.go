package database

import (
	"fmt"
	"log"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Connect() (*gorm.DB, error) {
	config := LoadConfig()

	log.Printf(
		"DB Config => host=%s port=%s user=%s db=%s",
		config.DBHost,
		config.DBPort,
		config.DBUser,
		config.DBName,
	)

	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		config.DBHost,
		config.DBPort,
		config.DBUser,
		config.DBPassword,
		config.DBName,
		config.DBSSLMode,
	)

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})

	if err != nil {
		return nil, err
	}

	log.Println("PostgreSQL connection established")

	return db, nil
}
