package config

import (
	"strings"
	"testing"
)

func setValidEnv(t *testing.T) {
	t.Helper()

	t.Setenv("DB_HOST", "localhost")
	t.Setenv("DB_PORT", "5432")
	t.Setenv("DB_USER", "aevor")
	t.Setenv("DB_PASSWORD", "test-password")
	t.Setenv("DB_NAME", "aevor_test")
	t.Setenv("GITHUB_CLIENT_ID", "test-client-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "test-client-secret")
	t.Setenv("GITHUB_REDIRECT_URL", "http://localhost:8080/auth/github/callback")
	t.Setenv("GITHUB_TOKEN_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("JWT_SECRET", "test-jwt-signing-secret-that-is-at-least-32-bytes")
}

func TestLoad_ValidConfiguration(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()

	if err != nil {
		t.Fatalf("Load() error with valid configuration: %v", err)
	}

	if len(cfg.JWTSecret) < 32 {
		t.Errorf("JWTSecret is %d bytes, want at least 32", len(cfg.JWTSecret))
	}
}

func TestLoad_MissingJWTSecretFailsSafely(t *testing.T) {
	setValidEnv(t)
	t.Setenv("JWT_SECRET", "")

	_, err := Load()

	if err == nil {
		t.Fatal("Load() succeeded with a missing JWT_SECRET")
	}

	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("error = %q, want it to name JWT_SECRET", err)
	}
}

func TestLoad_ShortJWTSecretFailsSafely(t *testing.T) {
	setValidEnv(t)
	t.Setenv("JWT_SECRET", "too-short")

	_, err := Load()

	if err == nil {
		t.Fatal("Load() succeeded with a short JWT_SECRET")
	}

	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("error = %q, want it to name JWT_SECRET", err)
	}
}
