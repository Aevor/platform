package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/pkg/config"
)

type tokenEndpoint struct {
	server   *httptest.Server
	requests int
	lastForm url.Values
	respond  func(w http.ResponseWriter, r *http.Request)
}

func newTokenEndpoint(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *tokenEndpoint {
	t.Helper()

	te := &tokenEndpoint{respond: respond}

	te.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		te.requests++

		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint could not parse form: %v", err)
		}

		te.lastForm = r.PostForm

		if te.respond != nil {
			te.respond(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fake-access-token","token_type":"bearer","scope":"read:user"}`))
	}))

	return te
}

func (te *tokenEndpoint) close() {
	te.server.Close()
}

type userEndpoint struct {
	server   *httptest.Server
	requests int
	lastAuth string
	lastUA   string
	respond  func(w http.ResponseWriter, r *http.Request)
}

func newUserEndpoint(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *userEndpoint {
	t.Helper()

	ue := &userEndpoint{respond: respond}

	ue.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ue.requests++
		ue.lastAuth = r.Header.Get("Authorization")
		ue.lastUA = r.Header.Get("User-Agent")

		if ue.respond != nil {
			ue.respond(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":583231,"login":"octocat","name":"The Octocat","email":"octocat@example.com","avatar_url":"https://avatars.githubusercontent.com/u/583231"}`))
	}))

	return ue
}

func (ue *userEndpoint) close() {
	ue.server.Close()
}

func newCallbackService(tokenURL, userURL string) *Service {
	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	ghClient := github.NewClient(nil, github.WithBaseURL(userURL))

	return NewService(&oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
		Scopes:       []string{"read:user"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, nil, nil, ghClient, nil)
}

func newCallbackRouter(handler *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/auth/github/callback", handler.GitHubCallback)

	return router
}

func loginForCallback(t *testing.T, handler *Handler) (*http.Cookie, oauthStateCookie, string) {
	t.Helper()

	router := newTestLoginRouter(handler)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusFound)
	}

	cookie := findCookie(t, rec, oauthCookieName)

	var stored oauthStateCookie

	if err := json.Unmarshal([]byte(oauthCookiePayload(t, rec)), &stored); err != nil {
		t.Fatalf("invalid oauth cookie payload: %v", err)
	}

	return cookie, stored, rec.Header().Get("Location")
}

func callCallback(router *gin.Engine, cookie *http.Cookie, query url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?"+query.Encode(), nil)

	if cookie != nil {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	return rec
}

func assertCallbackError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if body["error"] != code {
		t.Errorf("error = %q, want %q", body["error"], code)
	}
}

func assertOAuthCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	var cleared bool

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != oauthCookieName {
			continue
		}

		cleared = true

		if cookie.Value != "" {
			t.Errorf("oauth cookie value = %q, want empty", cookie.Value)
		}

		if cookie.MaxAge > 0 {
			t.Errorf("oauth cookie MaxAge = %d, want <= 0", cookie.MaxAge)
		}
	}

	if !cleared {
		t.Error("response does not contain a Set-Cookie clearing the oauth cookie")
	}
}

func TestCallback_ValidStateAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]interface{}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("body[status] = %v, want ok", body["status"])
	}

	user, ok := body["user"].(map[string]interface{})

	if !ok {
		t.Fatal("response is missing the user object")
	}

	if user["id"] != float64(583231) {
		t.Errorf("user.id = %v, want 583231", user["id"])
	}

	if user["login"] != "octocat" {
		t.Errorf("user.login = %v, want octocat", user["login"])
	}

	if te.requests != 1 {
		t.Errorf("token endpoint requests = %d, want 1", te.requests)
	}

	if te.lastForm.Get("code") != "valid-auth-code" {
		t.Errorf("exchange code = %q, want valid-auth-code", te.lastForm.Get("code"))
	}

	if te.lastForm.Get("grant_type") != "authorization_code" {
		t.Errorf("exchange grant_type = %q, want authorization_code", te.lastForm.Get("grant_type"))
	}

	if te.lastForm.Get("client_id") != "test-client-id" {
		t.Errorf("exchange client_id = %q, want test-client-id", te.lastForm.Get("client_id"))
	}

	if te.lastForm.Get("client_secret") != "test-client-secret" {
		t.Errorf("exchange client_secret = %q, want test-client-secret", te.lastForm.Get("client_secret"))
	}

	if te.lastForm.Get("redirect_uri") != "http://localhost:8080/auth/github/callback" {
		t.Errorf("exchange redirect_uri = %q, want the fixed server-side redirect URI", te.lastForm.Get("redirect_uri"))
	}

	if ue.requests != 1 {
		t.Errorf("user endpoint requests = %d, want 1", ue.requests)
	}

	if ue.lastAuth != "Bearer fake-access-token" {
		t.Errorf("user endpoint Authorization = %q, want the exchanged token as a Bearer credential", ue.lastAuth)
	}

	if ue.lastUA == "" || !strings.Contains(ue.lastUA, "Aevor") {
		t.Errorf("user endpoint User-Agent = %q, want an Aevor identifying User-Agent", ue.lastUA)
	}
}

func TestCallback_MissingStateCookieRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", "attacker-state")

	rec := callCallback(router, nil, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_state")

	if te.requests != 0 {
		t.Errorf("token endpoint requests = %d, want 0", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestCallback_MalformedStateCookieRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie := &http.Cookie{
		Name:  oauthCookieName,
		Value: url.QueryEscape(`{not-json`),
	}

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", "some-state")

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_state")

	if te.requests != 0 {
		t.Errorf("token endpoint requests = %d, want 0", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestCallback_MismatchedStateRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State+"tampered")

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_state")

	if te.requests != 0 {
		t.Errorf("token endpoint requests = %d, want 0", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestStateComparisonPerformedSafely(t *testing.T) {
	pairs := [][2]string{
		{"", ""},
		{"a", "a"},
		{"a", "b"},
		{"state", "state"},
		{"state", "states"},
		{"same-length", "different"},
		{"prefix-shared", "prefix-share2"},
		{"abcdefghijklmnopqrstuvwxyz0123456789-_", "abcdefghijklmnopqrstuvwxyz0123456789-_"},
	}

	for _, pair := range pairs {
		expected := subtle.ConstantTimeCompare([]byte(pair[0]), []byte(pair[1])) == 1

		if got := VerifyState(pair[0], pair[1]); got != expected {
			t.Errorf("VerifyState(%q, %q) = %v, want %v", pair[0], pair[1], got, expected)
		}
	}
}

func TestCallback_VerifierFromCookiePassedToExchange(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, loginURL := loginForCallback(t, handler)

	loginLocation, err := url.Parse(loginURL)

	if err != nil {
		t.Fatalf("invalid login Location: %v", err)
	}

	if challenge := oauth2.S256ChallengeFromVerifier(stored.Verifier); challenge != loginLocation.Query().Get("code_challenge") {
		t.Fatalf("cookie verifier does not match the S256 challenge sent at login")
	}

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if te.lastForm.Get("code_verifier") != stored.Verifier {
		t.Errorf("exchange code_verifier = %q, want the original verifier from the cookie %q", te.lastForm.Get("code_verifier"), stored.Verifier)
	}
}

func TestCallback_ExchangeOccursExactlyOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if te.requests != 1 {
		t.Errorf("token endpoint requests = %d, want exactly 1", te.requests)
	}

	if ue.requests != 1 {
		t.Errorf("user endpoint requests = %d, want exactly 1", ue.requests)
	}
}

func TestCallback_ExchangeFailureNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	})
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "single-use-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusInternalServerError, "github_unavailable")

	if te.requests != 1 {
		t.Errorf("token endpoint requests = %d, want exactly 1 (the exchange must never be retried)", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0 (no profile fetch after a failed exchange)", ue.requests)
	}
}

func TestCallback_ExchangeInvalidCodeMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
	})
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "used-or-expired-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_code")

	if te.requests != 1 {
		t.Errorf("token endpoint requests = %d, want exactly 1", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestCallback_AccessDeniedMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("error", "access_denied")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusUnauthorized, "github_authorization_denied")

	if te.requests != 0 {
		t.Errorf("token endpoint requests = %d, want 0", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestCallback_StateVerifiedBeforeErrorParam(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("error", "access_denied")
	query.Set("state", stored.State+"tampered")

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_state")
}

func TestCallback_MissingCodeMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_code")

	if te.requests != 0 {
		t.Errorf("token endpoint requests = %d, want 0", te.requests)
	}

	if ue.requests != 0 {
		t.Errorf("user endpoint requests = %d, want 0", ue.requests)
	}
}

func TestCallback_CookieClearedAfterProcessing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	assertOAuthCookieCleared(t, rec)
}

func TestCallback_CookieClearedOnInvalidState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State+"tampered")

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusBadRequest, "invalid_state")

	assertOAuthCookieCleared(t, rec)
}

func TestCallback_GitHubUserEndpointErrorMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusUnauthorized, "github_api_unauthorized")

	if te.requests != 1 {
		t.Errorf("token endpoint requests = %d, want 1", te.requests)
	}

	if ue.requests != 1 {
		t.Errorf("user endpoint requests = %d, want 1", ue.requests)
	}
}

func TestCallback_NoSensitiveValuesInLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))

	var logged bytes.Buffer

	oldWriter := gin.DefaultWriter
	gin.DefaultWriter = &logged

	defer func() {
		gin.DefaultWriter = oldWriter
	}()

	router := gin.New()
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipQueryString: true,
	}))
	router.GET("/auth/github/callback", handler.GitHubCallback)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "sensitive-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	output := logged.String()

	for _, sensitive := range []string{
		"sensitive-auth-code",
		stored.State,
		stored.Verifier,
		"fake-access-token",
		"test-client-secret",
	} {
		if strings.Contains(output, sensitive) {
			t.Errorf("log output contains sensitive value %q", sensitive)
		}
	}
}

func TestCallback_GitHubAccessTokenNotInResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("response contains the GitHub access token")
	}
}

func TestCallback_NoSensitiveMaterialInErrorResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	})
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	handler := NewHandler(newCallbackService(te.server.URL, ue.server.URL))
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	scenarios := []struct {
		name    string
		query   url.Values
		wantErr string
	}{
		{
			name: "invalid state",
			query: func() url.Values {
				q := url.Values{}
				q.Set("code", "code-value")
				q.Set("state", stored.State+"tampered")
				return q
			}(),
			wantErr: "invalid_state",
		},
		{
			name: "access denied",
			query: func() url.Values {
				q := url.Values{}
				q.Set("error", "access_denied")
				q.Set("state", stored.State)
				return q
			}(),
			wantErr: "github_authorization_denied",
		},
		{
			name: "missing code",
			query: func() url.Values {
				q := url.Values{}
				q.Set("state", stored.State)
				return q
			}(),
			wantErr: "invalid_code",
		},
		{
			name: "github unavailable",
			query: func() url.Values {
				q := url.Values{}
				q.Set("code", "code-value")
				q.Set("state", stored.State)
				return q
			}(),
			wantErr: "github_unavailable",
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			rec := callCallback(router, cookie, sc.query)

			var body map[string]string

			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON error body: %v", err)
			}

			if body["error"] != sc.wantErr {
				t.Fatalf("error = %q, want %q", body["error"], sc.wantErr)
			}

			raw := rec.Body.String()

			for _, sensitive := range []string{"code-value", stored.State, stored.Verifier, "test-client-secret", "fake-access-token"} {
				if strings.Contains(raw, sensitive) {
					t.Errorf("error response contains sensitive value %q", sensitive)
				}
			}
		})
	}
}

func TestNewGitHubOAuthConfig_ExchangeIsSingleRequestByConfig(t *testing.T) {
	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	oauthConfig := NewGitHubOAuthConfig(cfg)

	if oauthConfig.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		t.Errorf("Endpoint.AuthStyle = %v, want AuthStyleInParams to prevent oauth2's auth-style probe from re-POSTing the single-use code", oauthConfig.Endpoint.AuthStyle)
	}

	if oauthConfig.Endpoint.TokenURL != "https://github.com/login/oauth/access_token" {
		t.Errorf("Endpoint.TokenURL = %q, want the GitHub token endpoint", oauthConfig.Endpoint.TokenURL)
	}
}
