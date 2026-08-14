package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"

	"golang.org/x/oauth2"

	"github.com/Aevor/platform/services/api/pkg/config"
)

func newTestAuthService() *Service {
	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	return NewService(NewGitHubOAuthConfig(cfg), nil, nil, nil, nil)
}

func TestGenerateState_ReturnsNonEmptyValue(t *testing.T) {
	state, err := GenerateState()

	if err != nil {
		t.Fatalf("GenerateState() error: %v", err)
	}

	if state == "" {
		t.Fatal("GenerateState() returned an empty state")
	}

	raw, err := base64.RawURLEncoding.DecodeString(state)

	if err != nil {
		t.Fatalf("state is not valid raw-url base64: %v", err)
	}

	if len(raw) != 32 {
		t.Fatalf("state entropy is %d bytes, want 32", len(raw))
	}
}

func TestGenerateState_ProducesDifferentValues(t *testing.T) {
	seen := make(map[string]bool)

	for i := 0; i < 100; i++ {
		state, err := GenerateState()

		if err != nil {
			t.Fatalf("GenerateState() error: %v", err)
		}

		if seen[state] {
			t.Fatalf("GenerateState() produced duplicate state %q", state)
		}

		seen[state] = true
	}
}

func TestVerifyState(t *testing.T) {
	if !VerifyState("state-a", "state-a") {
		t.Error("VerifyState() = false for matching state")
	}

	if VerifyState("state-a", "state-b") {
		t.Error("VerifyState() = true for differing state")
	}

	if VerifyState("state-a", "") {
		t.Error("VerifyState() = true for empty actual state")
	}
}

func TestLoginURL_ProducesValidPKCEVerifier(t *testing.T) {
	service := newTestAuthService()

	_, _, verifier, err := service.LoginURL()

	if err != nil {
		t.Fatalf("LoginURL() error: %v", err)
	}

	if verifier == "" {
		t.Fatal("LoginURL() returned an empty verifier")
	}

	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length is %d characters, want RFC 7636 range [43, 128]", len(verifier))
	}

	raw, err := base64.RawURLEncoding.DecodeString(verifier)

	if err != nil {
		t.Fatalf("verifier is not valid raw-url base64: %v", err)
	}

	if len(raw) < 32 {
		t.Fatalf("verifier entropy is %d bytes, want at least 32", len(raw))
	}
}

func TestS256Challenge_DeterministicForSameVerifier(t *testing.T) {
	verifier := oauth2.GenerateVerifier()

	first := oauth2.S256ChallengeFromVerifier(verifier)
	second := oauth2.S256ChallengeFromVerifier(verifier)

	if first != second {
		t.Fatalf("S256 challenge is not deterministic: %q != %q", first, second)
	}

	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])

	if first != expected {
		t.Fatalf("challenge is not SHA-256 base64url: got %q want %q", first, expected)
	}
}

func TestS256Challenge_DiffersForDifferentVerifiers(t *testing.T) {
	first := oauth2.S256ChallengeFromVerifier("verifier-a")
	second := oauth2.S256ChallengeFromVerifier("verifier-b")

	if first == second {
		t.Fatal("different verifiers produced the same challenge")
	}
}

func TestLoginURL_ContainsOAuthParams(t *testing.T) {
	service := newTestAuthService()

	loginURL, state, verifier, err := service.LoginURL()

	if err != nil {
		t.Fatalf("LoginURL() error: %v", err)
	}

	u, err := url.Parse(loginURL)

	if err != nil {
		t.Fatalf("LoginURL() returned an invalid URL: %v", err)
	}

	query := u.Query()

	want := map[string]string{
		"client_id":             "test-client-id",
		"redirect_uri":          "http://localhost:8080/auth/github/callback",
		"response_type":         "code",
		"scope":                 "read:user",
		"state":                 state,
		"code_challenge":        oauth2.S256ChallengeFromVerifier(verifier),
		"code_challenge_method": "S256",
	}

	for param, expected := range want {
		if got := query.Get(param); got != expected {
			t.Errorf("query param %q = %q, want %q", param, got, expected)
		}
	}
}
