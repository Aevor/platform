package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func validRegisteredClaims(userID uuid.UUID) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Issuer:    jwtIssuer,
		Audience:  jwt.ClaimStrings{jwtAudience},
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(defaultTTL)),
	}
}

func withSubject(claims jwt.RegisteredClaims, subject string) jwt.RegisteredClaims {
	claims.Subject = subject

	return claims
}

func withIssuer(claims jwt.RegisteredClaims, issuer string) jwt.RegisteredClaims {
	claims.Issuer = issuer

	return claims
}

func withAudience(claims jwt.RegisteredClaims, audience string) jwt.RegisteredClaims {
	claims.Audience = jwt.ClaimStrings{audience}

	return claims
}

func signToken(t *testing.T, method jwt.SigningMethod, claims jwt.Claims, key any) string {
	t.Helper()

	token := jwt.NewWithClaims(method, claims)

	signed, err := token.SignedString(key)

	if err != nil {
		t.Fatalf("could not sign token with %s: %v", method.Alg(), err)
	}

	return signed
}

func noneToken(t *testing.T, claims jwt.RegisteredClaims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)

	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)

	if err != nil {
		t.Fatalf("could not sign token with none: %v", err)
	}

	return signed
}

func tamperToken(token string) string {
	parts := strings.Split(token, ".")

	if len(parts) != 3 {
		return token + "x"
	}

	mid := len(parts[2]) / 2

	replacement := byte('A')

	if parts[2][mid] == 'A' {
		replacement = 'B'
	}

	parts[2] = parts[2][:mid] + string(replacement) + parts[2][mid+1:]

	return strings.Join(parts, ".")
}

func echoAuthenticatedUserID(c *gin.Context) {
	userID, ok := GetAuthenticatedUserID(c)

	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "unauthorized",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id": userID.String(),
	})
}

func newProtectedRouter(manager *JWTManager) *gin.Engine {
	router := gin.New()
	router.GET("/protected", RequireAuth(manager), echoAuthenticatedUserID)

	return router
}

func performProtectedRequest(router *gin.Engine, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	return rec
}

func TestRequireAuth_ValidTokenSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	token, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := performProtectedRequest(router, "Bearer "+token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["user_id"] != userID.String() {
		t.Errorf("user_id = %q, want %q", body["user_id"], userID)
	}
}

func TestRequireAuth_InvalidTokensRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	expired, err := testJWTManagerAt(time.Now().Add(-2*time.Hour)).Issue(userID, -time.Hour)

	if err != nil {
		t.Fatalf("Issue() expired token error: %v", err)
	}

	valid, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() valid token error: %v", err)
	}

	tampered := tamperToken(valid)

	wrongKey := []byte("a-different-32-byte-signing-secret-value!")

	cases := []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"empty bearer value", "Bearer "},
		{"malformed token", "Bearer not-a-jwt"},
		{"tampered signature", "Bearer " + tampered},
		{"expired token", "Bearer " + expired},
		{
			"invalid signature",
			"Bearer " + signToken(t, jwt.SigningMethodHS256, validRegisteredClaims(userID), wrongKey),
		},
		{
			"missing sub",
			"Bearer " + signToken(t, jwt.SigningMethodHS256, jwt.RegisteredClaims{
				Issuer:    jwtIssuer,
				Audience:  jwt.ClaimStrings{jwtAudience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(defaultTTL)),
			}, testJWTSecret),
		},
		{
			"invalid sub",
			"Bearer " + signToken(t, jwt.SigningMethodHS256, withSubject(validRegisteredClaims(userID), "not-a-uuid"), testJWTSecret),
		},
		{
			"wrong issuer",
			"Bearer " + signToken(t, jwt.SigningMethodHS256, withIssuer(validRegisteredClaims(userID), "evil-issuer"), testJWTSecret),
		},
		{
			"wrong audience",
			"Bearer " + signToken(t, jwt.SigningMethodHS256, withAudience(validRegisteredClaims(userID), "evil-audience"), testJWTSecret),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := performProtectedRequest(router, tc.token)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}

			var body map[string]string

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON error body: %v", err)
			}

			if body["error"] != "unauthorized" {
				t.Errorf("error = %q, want %q", body["error"], "unauthorized")
			}
		})
	}
}

func TestRequireAuth_WrongSigningAlgorithmRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
		t.Run(method.Alg(), func(t *testing.T) {
			// Signed with the SAME secret — only the header algorithm differs.
			token := signToken(t, method, validRegisteredClaims(userID), testJWTSecret)

			rec := performProtectedRequest(router, "Bearer "+token)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d for alg %s (body %s)", rec.Code, http.StatusUnauthorized, method.Alg(), rec.Body.String())
			}
		})
	}
}

func TestRequireAuth_NoneAlgorithmRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	token := noneToken(t, validRegisteredClaims(userID))

	rec := performProtectedRequest(router, "Bearer "+token)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for alg=none (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestRequireAuth_AlgorithmConfusionRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)

	if err != nil {
		t.Fatalf("could not generate RSA key: %v", err)
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("could not generate EC key: %v", err)
	}

	// Cryptographically valid signatures under unsupported algorithms. The
	// middleware must reject them purely because HS256 is the only approved
	// algorithm — not because of claims or key problems.
	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
		key    any
	}{
		{"RS256", jwt.SigningMethodRS256, rsaKey},
		{"ES256", jwt.SigningMethodES256, ecKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := signToken(t, tc.method, validRegisteredClaims(userID), tc.key)

			rec := performProtectedRequest(router, "Bearer "+token)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d for alg %s (body %s)", rec.Code, http.StatusUnauthorized, tc.method.Alg(), rec.Body.String())
			}
		})
	}

	// Control: identical claims signed with HS256 must succeed — proving the
	// rejection above is the algorithm whitelist, not the claims or signature.
	control, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() control token error: %v", err)
	}

	if rec := performProtectedRequest(router, "Bearer "+control); rec.Code != http.StatusOK {
		t.Fatalf("control HS256 token rejected (body %s)", rec.Body.String())
	}
}

func TestRequireAuth_AuthenticatedUUIDReachesContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()

	userID := uuid.New()

	token, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	router := gin.New()
	router.GET("/protected", RequireAuth(manager), func(c *gin.Context) {
		value, ok := c.Get(string(UserIDKey))

		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "missing"})
			return
		}

		got, ok := value.(uuid.UUID)

		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "wrong_type"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user_id": got.String()})
	})

	rec := performProtectedRequest(router, "Bearer "+token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["user_id"] != userID.String() {
		t.Errorf("context user_id = %q, want %q", body["user_id"], userID)
	}
}

func TestRequireAuth_HandlerRetrievesAuthenticatedUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	token, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := performProtectedRequest(router, "Bearer "+token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["user_id"] != userID.String() {
		t.Errorf("GetAuthenticatedUserID = %q, want %q", body["user_id"], userID)
	}
}

func TestRequireAuth_UnauthenticatedRequestNeverReachesHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()

	called := false

	router := gin.New()
	router.GET("/protected", RequireAuth(manager), func(c *gin.Context) {
		called = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for _, authorization := range []string{"", "Bearer ", "Bearer garbage"} {
		rec := performProtectedRequest(router, authorization)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d for %q", rec.Code, http.StatusUnauthorized, authorization)
		}
	}

	if called {
		t.Error("protected handler was invoked without authentication")
	}
}

func TestRequireAuth_JWTNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldWriter := gin.DefaultWriter
	var logged bytes.Buffer
	gin.DefaultWriter = &logged

	defer func() {
		gin.DefaultWriter = oldWriter
	}()

	manager := newTestJWTManager()

	valid, err := manager.Issue(uuid.New(), defaultTTL)

	if err != nil {
		t.Fatalf("Issue() valid token error: %v", err)
	}

	invalid, err := manager.Issue(uuid.New(), defaultTTL)

	if err != nil {
		t.Fatalf("Issue() invalid token error: %v", err)
	}

	invalid = tamperToken(invalid)

	router := gin.New()
	router.Use(gin.Logger())
	router.GET("/protected", RequireAuth(manager), echoAuthenticatedUserID)

	for _, authorization := range []string{"Bearer " + valid, "Bearer " + invalid} {
		rec := performProtectedRequest(router, authorization)

		if rec.Code == http.StatusInternalServerError {
			t.Fatalf("unexpected internal error for %q", authorization)
		}
	}

	output := logged.String()

	if strings.Contains(output, valid) {
		t.Error("valid JWT appears in log output")
	}

	if strings.Contains(output, invalid) {
		t.Error("invalid JWT appears in log output")
	}
}

func TestRequireAuth_TokenNotInErrorResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	token, err := manager.Issue(uuid.New(), defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	tampered := tamperToken(token)

	rec := performProtectedRequest(router, "Bearer "+tampered)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), tampered) || strings.Contains(rec.Body.String(), token) {
		t.Error("error response contains JWT material")
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if body["error"] != "unauthorized" {
		t.Errorf("error = %q, want %q", body["error"], "unauthorized")
	}
}

func TestRequireAuth_ModifiedPayloadRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	userID := uuid.New()

	valid, err := manager.Issue(userID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	// Classic payload-tampering attack: swap in claims for a DIFFERENT
	// subject while keeping the original header and signature. The JWT must
	// be rejected because the signature no longer covers the payload.
	attackerClaims, err := json.Marshal(validRegisteredClaims(uuid.New()))

	if err != nil {
		t.Fatalf("could not marshal attacker claims: %v", err)
	}

	parts := strings.Split(valid, ".")

	if len(parts) != 3 {
		t.Fatalf("valid token is not a three-segment JWS: %q", valid)
	}

	parts[1] = base64.RawURLEncoding.EncodeToString(attackerClaims)

	forged := strings.Join(parts, ".")

	rec := performProtectedRequest(router, "Bearer "+forged)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for a modified payload (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if body["error"] != "unauthorized" {
		t.Errorf("error = %q, want %q", body["error"], "unauthorized")
	}
}

func TestRequireAuth_ClientInputCannotOverrideIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := newTestJWTManager()
	router := newProtectedRouter(manager)

	authenticated := uuid.New()
	attacker := uuid.New()

	token, err := manager.Issue(authenticated, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/protected?user_id="+attacker.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", attacker.String())

	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["user_id"] != authenticated.String() {
		t.Errorf("identity = %q, want the verified JWT sub %q", body["user_id"], authenticated)
	}
}

func TestGetAuthenticatedUserID_FailsSafelyWithoutMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/protected", func(c *gin.Context) {
		userID, ok := GetAuthenticatedUserID(c)

		if ok {
			c.JSON(http.StatusInternalServerError, gin.H{"user_id": userID.String()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rec := performProtectedRequest(router, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}
