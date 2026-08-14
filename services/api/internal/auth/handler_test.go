package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
)

func newTestLoginRouter(handler *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/auth/github/login", handler.GitHubLogin)

	return router
}

func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}

	t.Fatalf("cookie %q not present in response", name)

	return nil
}

func oauthCookiePayload(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	value, err := url.QueryUnescape(findCookie(t, rec, oauthCookieName).Value)

	if err != nil {
		t.Fatalf("invalid oauth cookie value: %v", err)
	}

	return value
}

func TestGitHubLogin_RedirectsToAuthorizationURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(newTestAuthService())
	router := newTestLoginRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}

	loginURL, err := url.Parse(rec.Header().Get("Location"))

	if err != nil {
		t.Fatalf("invalid Location header: %v", err)
	}

	query := loginURL.Query()

	if query.Get("state") == "" {
		t.Fatal("authorization URL is missing state")
	}

	if query.Get("code_challenge") == "" {
		t.Fatal("authorization URL is missing code_challenge")
	}

	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", query.Get("code_challenge_method"))
	}

	if query.Get("client_id") != "test-client-id" {
		t.Fatalf("client_id = %q, want test-client-id", query.Get("client_id"))
	}
}

func TestGitHubLogin_SetsOAuthCookieAttributes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(newTestAuthService())
	router := newTestLoginRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	cookie := findCookie(t, rec, oauthCookieName)

	if !cookie.HttpOnly {
		t.Error("oauth cookie is not HttpOnly")
	}

	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("oauth cookie SameSite = %v, want Lax", cookie.SameSite)
	}

	if cookie.MaxAge != oauthCookieMaxAge {
		t.Errorf("oauth cookie MaxAge = %d, want %d", cookie.MaxAge, oauthCookieMaxAge)
	}

	if cookie.MaxAge > 600 {
		t.Errorf("oauth cookie MaxAge = %d exceeds the short-lived limit", cookie.MaxAge)
	}

	if cookie.Path != oauthCookiePath {
		t.Errorf("oauth cookie Path = %q, want %q", cookie.Path, oauthCookiePath)
	}

	if cookie.Secure {
		t.Error("oauth cookie is Secure; expected non-Secure for localhost development")
	}
}

func TestGitHubLogin_StateAndChallengeMatchCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(newTestAuthService())
	router := newTestLoginRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	loginURL, err := url.Parse(rec.Header().Get("Location"))

	if err != nil {
		t.Fatalf("invalid Location header: %v", err)
	}

	query := loginURL.Query()

	var stored oauthStateCookie

	if err := json.Unmarshal([]byte(oauthCookiePayload(t, rec)), &stored); err != nil {
		t.Fatalf("invalid oauth cookie payload: %v", err)
	}

	if stored.State != query.Get("state") {
		t.Errorf("cookie state %q does not match URL state %q", stored.State, query.Get("state"))
	}

	if stored.Verifier == "" {
		t.Fatal("cookie verifier is empty")
	}

	if challenge := oauth2.S256ChallengeFromVerifier(stored.Verifier); challenge != query.Get("code_challenge") {
		t.Errorf("URL code_challenge %q does not match S256 of cookie verifier (got %q)", query.Get("code_challenge"), challenge)
	}
}

func TestGitHubLogin_DoesNotLogSensitiveValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oldWriter := gin.DefaultWriter
	var logged bytes.Buffer
	gin.DefaultWriter = &logged

	defer func() {
		gin.DefaultWriter = oldWriter
	}()

	handler := NewHandler(newTestAuthService())

	router := gin.New()
	router.Use(gin.Logger())
	router.GET("/auth/github/login", handler.GitHubLogin)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	var stored oauthStateCookie

	if err := json.Unmarshal([]byte(oauthCookiePayload(t, rec)), &stored); err != nil {
		t.Fatalf("invalid oauth cookie payload: %v", err)
	}

	loginURL, err := url.Parse(rec.Header().Get("Location"))

	if err != nil {
		t.Fatalf("invalid Location header: %v", err)
	}

	output := logged.String()

	for _, sensitive := range []string{stored.State, stored.Verifier, loginURL.Query().Get("code_challenge")} {
		if strings.Contains(output, sensitive) {
			t.Errorf("log output contains sensitive oauth value %q", sensitive)
		}
	}
}
