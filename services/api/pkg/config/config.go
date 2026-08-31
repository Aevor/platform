package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

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

	// FrontendURL is the base URL of the Aevor frontend (FRONTEND_URL).
	// After a successful GitHub OAuth callback the API redirects the browser
	// here (e.g. http://localhost:5173) so the SPA can capture the JWT.
	FrontendURL string
	// GitHubBaseURL optionally overrides the GitHub REST API base URL
	// (GITHUB_API_BASE_URL). Empty means the production default. Used for
	// integration testing against a local mock GitHub.
	GitHubBaseURL string

	JWTSecret []byte

	GitHubTokenEncryptionKey []byte

	// WorkspaceRoot is the controlled filesystem root under which every
	// cloned repository workspace lives (WORKSPACE_ROOT). Required and
	// validated at startup: the API never writes workspaces to arbitrary
	// locations.
	WorkspaceRoot string

	// CloneAllowedHosts is the https-host allowlist for Git clone targets
	// (CLONE_ALLOWED_HOSTS, comma-separated; default "github.com").
	CloneAllowedHosts []string

	// CloneAllowFileTransport opts in to file:// clone URLs for local
	// development and integration verification only
	// (CLONE_ALLOW_FILE_URLS=true). Production keeps it false.
	CloneAllowFileTransport bool

	// FilterMaxFileSize is the per-file inclusion limit for codebase
	// filtering (FILTER_MAX_FILE_SIZE, bytes). Zero means the filtering
	// package default (1 MiB).
	FilterMaxFileSize int64

	// FilterMaxTotalBytes bounds the total size of selected content
	// (FILTER_MAX_TOTAL_BYTES, bytes). Zero means the package default
	// (32 MiB).
	FilterMaxTotalBytes int64

	// FilterMaxFiles bounds how many files may be included in one filtering
	// result (FILTER_MAX_FILES). Zero means the package default (5000).
	FilterMaxFiles int

	// AIServiceURL is the base URL of the external AI analysis service
	// (AI_SERVICE_URL). Defaults to http://localhost:11434 when empty.
	AIServiceURL string

	// AIServiceAPIKey is the server-side API key sent to the AI service
	// (AI_SERVICE_API_KEY). Empty means no Authorization header is sent.
	AIServiceAPIKey string
}

func Load() (*AppConfig, error) {
	_ = godotenv.Load()

	encryptionKey, err := decodeHex(os.Getenv("GITHUB_TOKEN_ENCRYPTION_KEY"))

	if err != nil {
		return nil, err
	}

	filterMaxFileSize, err := loadPositiveInt64("FILTER_MAX_FILE_SIZE")

	if err != nil {
		return nil, err
	}

	filterMaxTotalBytes, err := loadPositiveInt64("FILTER_MAX_TOTAL_BYTES")

	if err != nil {
		return nil, err
	}

	filterMaxFiles, err := loadPositiveInt("FILTER_MAX_FILES")

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
		FrontendURL:              os.Getenv("FRONTEND_URL"),
		GitHubBaseURL:            os.Getenv("GITHUB_API_BASE_URL"),
		JWTSecret:                []byte(os.Getenv("JWT_SECRET")),
		GitHubTokenEncryptionKey: encryptionKey,
		WorkspaceRoot:            os.Getenv("WORKSPACE_ROOT"),
		CloneAllowedHosts:        splitCSV(os.Getenv("CLONE_ALLOWED_HOSTS")),
		CloneAllowFileTransport:  os.Getenv("CLONE_ALLOW_FILE_URLS") == "true",
		FilterMaxFileSize:        filterMaxFileSize,
		FilterMaxTotalBytes:      filterMaxTotalBytes,
		FilterMaxFiles:           filterMaxFiles,
		AIServiceURL:             os.Getenv("AI_SERVICE_URL"),
		AIServiceAPIKey:          os.Getenv("AI_SERVICE_API_KEY"),
	}

	if len(cfg.CloneAllowedHosts) == 0 {
		cfg.CloneAllowedHosts = []string{"github.com"}
	}

	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	if cfg.DBSSLMode == "" {
		cfg.DBSSLMode = "disable"
	}

	if cfg.AIServiceURL == "" {
		cfg.AIServiceURL = "http://localhost:11434"
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

	if c.FrontendURL == "" {
		return fmt.Errorf("FRONTEND_URL is required (base URL the API redirects the SPA to after OAuth)")
	}

	if len(c.JWTSecret) < 32 {
		return fmt.Errorf("JWT_SECRET must be at least 32 bytes")
	}

	if len(c.GitHubTokenEncryptionKey) != 32 {
		return fmt.Errorf("GITHUB_TOKEN_ENCRYPTION_KEY must be a 32-byte hex string (64 hex characters)")
	}

	if c.WorkspaceRoot == "" {
		return fmt.Errorf("WORKSPACE_ROOT is required (controlled directory for repository workspaces)")
	}

	return nil
}

func loadPositiveInt64(name string) (int64, error) {
	raw := os.Getenv(name)

	if raw == "" {
		return 0, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)

	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer when set", name)
	}

	return value, nil
}

func loadPositiveInt(name string) (int, error) {
	raw := os.Getenv(name)

	if raw == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(raw)

	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer when set", name)
	}

	return value, nil
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))

	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}

	return result
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
