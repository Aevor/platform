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
	t.Setenv("WORKSPACE_ROOT", t.TempDir())
	t.Setenv("CLONE_ALLOWED_HOSTS", "")
	t.Setenv("CLONE_ALLOW_FILE_URLS", "")
	t.Setenv("FILTER_MAX_FILE_SIZE", "")
	t.Setenv("FILTER_MAX_TOTAL_BYTES", "")
	t.Setenv("FILTER_MAX_FILES", "")
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

	if cfg.WorkspaceRoot == "" {
		t.Errorf("WorkspaceRoot is empty, want the controlled workspace directory")
	}

	if len(cfg.CloneAllowedHosts) != 1 || cfg.CloneAllowedHosts[0] != "github.com" {
		t.Errorf("CloneAllowedHosts = %v, want default [github.com]", cfg.CloneAllowedHosts)
	}

	if cfg.CloneAllowFileTransport {
		t.Errorf("CloneAllowFileTransport = true by default, want false")
	}
}

func TestLoad_MissingWorkspaceRootFailsSafely(t *testing.T) {
	setValidEnv(t)

	t.Setenv("WORKSPACE_ROOT", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without WORKSPACE_ROOT")
	} else if !strings.Contains(err.Error(), "WORKSPACE_ROOT") {
		t.Errorf("error = %q, want it to name WORKSPACE_ROOT", err)
	}
}

func TestLoad_CloneHostsAndFileTransportFromEnv(t *testing.T) {
	setValidEnv(t)

	t.Setenv("CLONE_ALLOWED_HOSTS", "github.com, gitlab.com ")
	t.Setenv("CLONE_ALLOW_FILE_URLS", "true")

	cfg, err := Load()

	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.CloneAllowedHosts) != 2 || cfg.CloneAllowedHosts[0] != "github.com" || cfg.CloneAllowedHosts[1] != "gitlab.com" {
		t.Errorf("CloneAllowedHosts = %v, want [github.com gitlab.com]", cfg.CloneAllowedHosts)
	}

	if !cfg.CloneAllowFileTransport {
		t.Errorf("CloneAllowFileTransport = false, want true from CLONE_ALLOW_FILE_URLS=true")
	}
}

func TestLoad_FilterLimitsFromEnv(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()

	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.FilterMaxFileSize != 0 || cfg.FilterMaxTotalBytes != 0 || cfg.FilterMaxFiles != 0 {
		t.Errorf("unset filter knobs must be zero (package defaults), got %+v", cfg)
	}

	t.Setenv("FILTER_MAX_FILE_SIZE", "262144")
	t.Setenv("FILTER_MAX_TOTAL_BYTES", "1048576")
	t.Setenv("FILTER_MAX_FILES", "100")

	cfg, err = Load()

	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.FilterMaxFileSize != 262144 || cfg.FilterMaxTotalBytes != 1048576 || cfg.FilterMaxFiles != 100 {
		t.Errorf("filter knobs = %+v, want parsed values", cfg)
	}
}

func TestLoad_InvalidFilterLimitsFailSafely(t *testing.T) {
	cases := map[string]map[string]string{
		"negative file size": {"FILTER_MAX_FILE_SIZE": "-1"},
		"zero total bytes":   {"FILTER_MAX_TOTAL_BYTES": "0"},
		"non-numeric files":  {"FILTER_MAX_FILES": "many"},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)

			for key, value := range env {
				t.Setenv(key, value)
			}

			_, err := Load()

			if err == nil {
				t.Fatalf("Load() succeeded with invalid %v", env)
			}

			for key := range env {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error = %q, want it to name %s", err, key)
				}
			}
		})
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
