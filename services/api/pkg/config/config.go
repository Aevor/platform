package config

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type AppConfig struct {
	Port string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string

	JWTSecret []byte

	GitHubTokenEncryptionKey []byte
}

func Load() (*AppConfig, error) {
	_ = godotenv.Load()

	encryptionKey, err := decodeHex(os.Getenv("GITHUB_TOKEN_ENCRYPTION_KEY"))

	if err != nil {
		return nil, err
	}

	cfg := &AppConfig{
		Port:                     os.Getenv("PORT"),
		DBHost:                   os.Getenv("DB_HOST"),
		DBPort:                   os.Getenv("DB_PORT"),
		DBUser:                   os.Getenv("DB_USER"),
		DBPassword:               os.Getenv("DB_PASSWORD"),
		DBName:                   os.Getenv("DB_NAME"),
		DBSSLMode:                os.Getenv("DB_SSLMODE"),
		GitHubClientID:           os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret:       os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubRedirectURL:        os.Getenv("GITHUB_REDIRECT_URL"),
		JWTSecret:                []byte(os.Getenv("JWT_SECRET")),
		GitHubTokenEncryptionKey: encryptionKey,
	}

	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	if cfg.DBSSLMode == "" {
		cfg.DBSSLMode = "disable"
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *AppConfig) validate() error {
	if c.DBHost == "" || c.DBPort == "" || c.DBUser == "" || c.DBPassword == "" || c.DBName == "" {
		return fmt.Errorf("database configuration is incomplete (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME are required)")
	}

	if c.GitHubClientID == "" || c.GitHubClientSecret == "" || c.GitHubRedirectURL == "" {
		return fmt.Errorf("github oauth configuration is incomplete (GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET, GITHUB_REDIRECT_URL are required)")
	}

	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("JWT_SECRET must be at least 32 bytes")
	}

	if len(c.GitHubTokenEncryptionKey) != 32 {
		return fmt.Errorf("GITHUB_TOKEN_ENCRYPTION_KEY must be a 32-byte hex string (64 hex characters)")
	}

	return nil
}

func decodeHex(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}

	decoded, err := hex.DecodeString(raw)

	if err != nil {
		return nil, fmt.Errorf("GITHUB_TOKEN_ENCRYPTION_KEY is not valid hex (expected a 32-byte key as 64 hex characters)")
	}

	return decoded, nil
}
