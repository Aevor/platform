package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func testJWTManagerAt(at time.Time) *JWTManager {
	return NewJWTManager(testJWTSecret, WithClock(func() time.Time { return at }))
}

func parsedClaims(t *testing.T, token string) *jwt.RegisteredClaims {
	t.Helper()

	claims := &jwt.RegisteredClaims{}

	// Several tests intentionally issue tokens at a FIXED past instant to
	// prove clock injection works; parsing them with live wall-clock
	// validation makes those tests time bombs that start failing once
	// defaultTTL has elapsed. This helper only inspects claim values —
	// validity semantics are covered by manager.Verify and middleware tests.
	_, err := jwt.ParseWithClaims(
		token,
		claims,
		func(token *jwt.Token) (any, error) {
			return testJWTSecret, nil
		},
		jwt.WithoutClaimsValidation(),
	)

	if err != nil {
		t.Fatalf("could not parse JWT claims: %v", err)
	}

	return claims
}

func decodedJWTClaims(t *testing.T, token string) string {
	t.Helper()

	parts := strings.Split(token, ".")

	if len(parts) != 3 {
		t.Fatalf("token is not a JWS with three segments: %q", token)
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])

	if err != nil {
		t.Fatalf("could not decode JWT payload: %v", err)
	}

	return string(raw)
}

func TestJWT_ValidUserIDProducesValidToken(t *testing.T) {
	manager := NewJWTManager(testJWTSecret)

	userID := uuid.New()

	token, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	if token == "" {
		t.Fatal("Issue() returned an empty token")
	}

	verified, err := manager.Verify(token)

	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if verified != userID {
		t.Errorf("Verify() = %q, want %q", verified, userID)
	}
}

func TestJWT_ContainsCorrectSubject(t *testing.T) {
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	token, err := testJWTManagerAt(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	claims := parsedClaims(t, token)

	if claims.Subject != userID.String() {
		t.Errorf("sub = %q, want the Aevor user UUID %q", claims.Subject, userID)
	}
}

func TestJWT_ContainsCorrectIssuer(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	claims := parsedClaims(t, token)

	if claims.Issuer != jwtIssuer {
		t.Errorf("iss = %q, want %q", claims.Issuer, jwtIssuer)
	}
}

func TestJWT_ContainsCorrectAudience(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	claims := parsedClaims(t, token)

	var hasAudience bool

	for _, a := range claims.Audience {
		if a == jwtAudience {
			hasAudience = true
			break
		}
	}

	if !hasAudience {
		t.Errorf("aud = %v, want it to contain %q", claims.Audience, jwtAudience)
	}
}

func TestJWT_ContainsCorrectIssuedAt(t *testing.T) {
	issuedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	userID := uuid.New()

	token, err := testJWTManagerAt(issuedAt).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	claims := parsedClaims(t, token)

	if claims.IssuedAt == nil {
		t.Fatal("iat claim is missing")
	}

	if !claims.IssuedAt.Time.Equal(issuedAt) {
		t.Errorf("iat = %v, want %v", claims.IssuedAt.Time, issuedAt)
	}
}

func TestJWT_ContainsCorrectExpiration(t *testing.T) {
	issuedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	userID := uuid.New()

	token, err := testJWTManagerAt(issuedAt).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	claims := parsedClaims(t, token)

	if claims.ExpiresAt == nil {
		t.Fatal("exp claim is missing")
	}

	wantExp := issuedAt.Add(defaultTTL)

	if !claims.ExpiresAt.Time.Equal(wantExp) {
		t.Errorf("exp = %v, want %v (iat + 7 days)", claims.ExpiresAt.Time, wantExp)
	}
}

func TestJWT_NoJWTIDClaim(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	payload := decodedJWTClaims(t, token)

	if strings.Contains(payload, `"jti"`) {
		t.Error("JWT contains a jti claim; the approved design does not require one")
	}
}

func TestJWT_UsesApprovedSigningAlgorithm(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	parser := jwt.NewParser()

	parsed, _, err := parser.ParseUnverified(token, jwt.MapClaims{})

	if err != nil {
		t.Fatalf("could not inspect JWT header: %v", err)
	}

	if parsed.Method == nil {
		t.Fatal("JWT header is missing the signing algorithm")
	}

	if parsed.Method.Alg() != jwt.SigningMethodHS256.Alg() {
		t.Errorf("JWT alg = %q, want HS256 (approved by design D5)", parsed.Method.Alg())
	}
}

func TestJWT_VerifiesWithConfiguredKey(t *testing.T) {
	manager := NewJWTManager(testJWTSecret)

	userID := uuid.New()

	token, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	if _, err := manager.Verify(token); err != nil {
		t.Errorf("Verify() error with the configured key: %v", err)
	}
}

func TestJWT_WrongKeyFailsVerification(t *testing.T) {
	issuer := NewJWTManager(testJWTSecret)
	other := NewJWTManager([]byte("a-different-32-byte-signing-secret-value!"))

	token, err := issuer.Issue(uuid.New(), defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	if _, err := other.Verify(token); err == nil {
		t.Error("Verify() succeeded with a different signing key")
	}
}

func TestJWT_ExpiredTokenFailsVerification(t *testing.T) {
	manager := NewJWTManager(testJWTSecret)

	token, err := manager.Issue(uuid.New(), -time.Hour)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	if _, err := manager.Verify(token); err == nil {
		t.Error("Verify() accepted an expired JWT")
	}
}

func TestJWTManager_ShortSecretRejected(t *testing.T) {
	manager := NewJWTManager([]byte("too-short"))

	if _, err := manager.Issue(uuid.New(), defaultTTL); err == nil {
		t.Error("Issue() succeeded with a short signing secret")
	}
}

func TestJWT_DoesNotContainGitHubAccessToken(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	payload := decodedJWTClaims(t, token)

	for _, sensitive := range []string{"ghs_", "gho_", "ghp_", "fake-access-token", "access_token"} {
		if strings.Contains(payload, sensitive) {
			t.Errorf("JWT payload contains GitHub access token material %q", sensitive)
		}
	}
}

func TestJWT_DoesNotContainPKCEVerifier(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	payload := decodedJWTClaims(t, token)

	for _, sensitive := range []string{"code_verifier", "verifier"} {
		if strings.Contains(payload, sensitive) {
			t.Errorf("JWT payload contains PKCE verifier material %q", sensitive)
		}
	}
}

func TestJWT_DoesNotContainOAuthState(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	payload := decodedJWTClaims(t, token)

	if strings.Contains(payload, "state") {
		t.Error("JWT payload contains OAuth state material")
	}
}

func TestJWT_DoesNotContainUserProfileData(t *testing.T) {
	userID := uuid.New()

	token, err := testJWTManagerAt(time.Now()).Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	payload := decodedJWTClaims(t, token)

	for _, sensitive := range []string{
		"octocat@example.com",
		"octocat",
		"The Octocat",
		"avatar",
		"github_id",
		"display_name",
	} {
		if strings.Contains(payload, sensitive) {
			t.Errorf("JWT payload contains user profile data %q", sensitive)
		}
	}
}

func TestJWT_IndependentTokensHaveIndependentTiming(t *testing.T) {
	start := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	current := start

	manager := NewJWTManager(testJWTSecret, WithClock(func() time.Time { return current }))

	userID := uuid.New()

	first, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("first Issue() error: %v", err)
	}

	current = current.Add(time.Hour)

	second, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("second Issue() error: %v", err)
	}

	firstClaims := parsedClaims(t, first)
	secondClaims := parsedClaims(t, second)

	if firstClaims.IssuedAt == nil || !firstClaims.IssuedAt.Time.Equal(start) {
		t.Errorf("first token iat = %v, want %v", firstClaims.IssuedAt, start)
	}

	if secondClaims.IssuedAt == nil || !secondClaims.IssuedAt.Time.Equal(start.Add(time.Hour)) {
		t.Errorf("second token iat = %v, want %v", secondClaims.IssuedAt, start.Add(time.Hour))
	}

	if firstClaims.IssuedAt.Unix() == secondClaims.IssuedAt.Unix() {
		t.Error("independently issued JWTs share the same iat")
	}

	for i, claims := range []*jwt.RegisteredClaims{firstClaims, secondClaims} {
		gotLifetime := claims.ExpiresAt.Unix() - claims.IssuedAt.Unix()

		if gotLifetime != int64(defaultTTL.Seconds()) {
			t.Errorf("token %d lifetime = %d seconds, want %d", i+1, gotLifetime, int64(defaultTTL.Seconds()))
		}
	}
}
