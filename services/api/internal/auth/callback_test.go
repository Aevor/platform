package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/pkg/config"
)

var testEncryptionKey = func() []byte {
	key, err := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	if err != nil {
		panic(err)
	}

	return key
}()

var testJWTSecret = []byte("test-jwt-signing-secret-that-is-at-least-32-bytes")

func newTestJWTManager() *JWTManager {
	return NewJWTManager(testJWTSecret)
}

type fakeUserRepository struct {
	users     map[int64]*users.User
	creates   []*users.User
	upserts   []*users.User
	upsertErr error
}

func newFakeUserRepository() *fakeUserRepository {
	return &fakeUserRepository{
		users: make(map[int64]*users.User),
	}
}

func (f *fakeUserRepository) Create(user *users.User) error {
	user.ID = uuid.New()
	f.creates = append(f.creates, user)
	f.users[user.GithubID] = user

	return nil
}

func (f *fakeUserRepository) GetByID(id uuid.UUID) (*users.User, error) {
	for _, user := range f.users {
		if user.ID == id {
			return user, nil
		}
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeUserRepository) GetByGitHubID(githubID int64) (*users.User, error) {
	if user, ok := f.users[githubID]; ok {
		return user, nil
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeUserRepository) Update(user *users.User) error {
	f.users[user.GithubID] = user

	return nil
}

func (f *fakeUserRepository) UpsertByGitHubID(user *users.User) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}

	if existing, ok := f.users[user.GithubID]; ok {
		user.ID = existing.ID
		user.CreatedAt = existing.CreatedAt
		*existing = *user
		f.upserts = append(f.upserts, user)

		return nil
	}

	return f.Create(user)
}

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

func newCallbackService(tokenURL, userURL string) (*Service, *fakeUserRepository) {
	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	ghClient := github.NewClient(nil, github.WithBaseURL(userURL))

	userRepo := newFakeUserRepository()
	userService := users.NewService(userRepo)

	return NewService(&oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
		Scopes:       []string{"read:user"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, userService, newTestJWTManager(), ghClient, testEncryptionKey), userRepo
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

	service, repo := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	authToken, ok := body["token"].(string)

	if !ok || authToken == "" {
		t.Fatal("callback response is missing the Aevor JWT")
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("callback response contains the GitHub access token")
	}

	user, ok := body["user"].(map[string]interface{})

	if !ok {
		t.Fatal("response is missing the user object")
	}

	if _, err := uuid.Parse(user["id"].(string)); err != nil {
		t.Errorf("user.id = %v is not a valid Aevor UUID: %v", user["id"], err)
	}

	userID, err := newTestJWTManager().Verify(authToken)

	if err != nil {
		t.Fatalf("callback JWT does not verify: %v", err)
	}

	if userID.String() != user["id"] {
		t.Errorf("callback JWT sub = %q, response user.id = %v (must match)", userID, user["id"])
	}

	if user["github_id"] != float64(583231) {
		t.Errorf("user.github_id = %v, want 583231", user["github_id"])
	}

	if user["username"] != "octocat" {
		t.Errorf("user.username = %v, want octocat", user["username"])
	}

	if user["display_name"] != "The Octocat" {
		t.Errorf("user.display_name = %v, want The Octocat", user["display_name"])
	}

	if user["email"] != "octocat@example.com" {
		t.Errorf("user.email = %v, want octocat@example.com", user["email"])
	}

	if user["avatar_url"] != "https://avatars.githubusercontent.com/u/583231" {
		t.Errorf("user.avatar_url = %v, want the avatar URL", user["avatar_url"])
	}

	storedUser := repo.users[583231]

	if storedUser == nil {
		t.Fatal("no user row stored for github_id 583231")
	}

	if storedUser.ID.String() != user["id"] {
		t.Errorf("stored Aevor UUID = %q, response UUID = %v (must match)", storedUser.ID, user["id"])
	}

	if storedUser.GitHubAccessToken == nil {
		t.Fatal("stored user has no GitHubAccessToken")
	}

	if *storedUser.GitHubAccessToken == "fake-access-token" {
		t.Error("plaintext GitHub access token was persisted")
	}

	decrypted, err := users.Decrypt(*storedUser.GitHubAccessToken, testEncryptionKey)

	if err != nil {
		t.Fatalf("stored token does not decrypt: %v", err)
	}

	if decrypted != "fake-access-token" {
		t.Errorf("stored token decrypts to %q, want the GitHub access token", decrypted)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)

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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
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

func TestCallback_ReLoginReplacesTokenKeepsAevorUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokens := []string{"ghs_first-token", "ghs_second-token"}
	i := 0

	te := newTokenEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"` + tokens[i] + `","token_type":"bearer","scope":"read:user"}`))

		if i < len(tokens)-1 {
			i++
		}
	})
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	service, repo := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	first := callCallback(router, cookie, query)

	if first.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, want %d (body %s)", first.Code, http.StatusOK, first.Body.String())
	}

	var firstBody struct {
		User users.UserResponse `json:"user"`
	}

	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("invalid first callback JSON: %v", err)
	}

	second := callCallback(router, cookie, query)

	if second.Code != http.StatusOK {
		t.Fatalf("second callback status = %d, want %d (body %s)", second.Code, http.StatusOK, second.Body.String())
	}

	var secondBody struct {
		User users.UserResponse `json:"user"`
	}

	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("invalid second callback JSON: %v", err)
	}

	if firstBody.User.ID != secondBody.User.ID {
		t.Errorf("Aevor UUID changed across re-login: %q -> %q (must stay stable)", firstBody.User.ID, secondBody.User.ID)
	}

	if len(repo.users) != 1 {
		t.Errorf("stored users = %d, want 1 across two logins", len(repo.users))
	}

	if len(repo.creates) != 1 {
		t.Errorf("user Create() calls = %d, want 1 (second login must upsert)", len(repo.creates))
	}

	if len(repo.upserts) != 1 {
		t.Errorf("user Upsert() calls = %d, want 1 on re-login", len(repo.upserts))
	}

	storedUser := repo.users[583231]

	if storedUser == nil {
		t.Fatal("no user row stored for github_id 583231")
	}

	if storedUser.GitHubAccessToken == nil {
		t.Fatal("stored user has no GitHubAccessToken after re-login")
	}

	decrypted, err := users.Decrypt(*storedUser.GitHubAccessToken, testEncryptionKey)

	if err != nil {
		t.Fatalf("stored token does not decrypt after re-login: %v", err)
	}

	if decrypted != "ghs_second-token" {
		t.Errorf("stored token decrypts to %q, want ghs_second-token (re-login replaces the token)", decrypted)
	}
}

func TestCallback_EncryptionFailureDoesNotPersist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	ghClient := github.NewClient(nil, github.WithBaseURL(ue.server.URL))

	userRepo := newFakeUserRepository()
	userService := users.NewService(userRepo)

	service := NewService(&oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
		Scopes:       []string{"read:user"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  te.server.URL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, userService, nil, ghClient, []byte("too-short"))
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusInternalServerError, "internal")

	if len(userRepo.users) != 0 {
		t.Errorf("stored users = %d, want 0 (failed encryption must not persist anything)", len(userRepo.users))
	}

	if len(userRepo.creates) != 0 {
		t.Errorf("user Create() calls = %d, want 0 (failed encryption must not persist anything)", len(userRepo.creates))
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("error response contains the GitHub access token")
	}
}

func TestCallback_DatabaseFailureDoesNotLeakToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	service, repo := newCallbackService(te.server.URL, ue.server.URL)
	repo.upsertErr = errors.New("database connection lost")
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusInternalServerError, "internal")

	if len(repo.users) != 0 {
		t.Errorf("stored users = %d, want 0 on a failed upsert", len(repo.users))
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("error response contains the GitHub access token")
	}

	if strings.Contains(rec.Body.String(), "token") {
		t.Error("error response contains a token field on a failed upsert")
	}
}

func TestCallback_UserRowIncludesEncryptedTokenNotPlaintext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	service, repo := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	storedUser := repo.users[583231]

	if storedUser == nil {
		t.Fatal("no user row stored for github_id 583231")
	}

	if storedUser.GitHubAccessToken == nil {
		t.Fatal("stored user has no GitHubAccessToken")
	}

	if strings.Contains(*storedUser.GitHubAccessToken, "fake-access-token") {
		t.Error("stored value contains the plaintext GitHub access token")
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("callback response contains the GitHub access token")
	}
}

func TestCallback_FailedGitHubAuthProducesNoJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})
	defer ue.close()

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusUnauthorized, "github_api_unauthorized")

	var body map[string]interface{}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if _, ok := body["token"]; ok {
		t.Error("failed GitHub authentication still produced a JWT")
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("error response contains the GitHub access token")
	}
}

func TestCallback_JWTSigningFailureHandledSafely(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	cfg := &config.AppConfig{
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "http://localhost:8080/auth/github/callback",
	}

	ghClient := github.NewClient(nil, github.WithBaseURL(ue.server.URL))

	userRepo := newFakeUserRepository()
	userService := users.NewService(userRepo)

	service := NewService(&oauth2.Config{
		ClientID:     cfg.GitHubClientID,
		ClientSecret: cfg.GitHubClientSecret,
		RedirectURL:  cfg.GitHubRedirectURL,
		Scopes:       []string{"read:user"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  te.server.URL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, userService, NewJWTManager([]byte("too-short")), ghClient, testEncryptionKey)
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	assertCallbackError(t, rec, http.StatusInternalServerError, "internal")

	var body map[string]interface{}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if _, ok := body["token"]; ok {
		t.Error("JWT signing failure still emitted a token")
	}

	if strings.Contains(rec.Body.String(), "fake-access-token") {
		t.Error("error response contains the GitHub access token")
	}
}

func TestCallback_JWTClaimsCarryOnlyAevorIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	te := newTokenEndpoint(t, nil)
	defer te.close()

	ue := newUserEndpoint(t, nil)
	defer ue.close()

	service, _ := newCallbackService(te.server.URL, ue.server.URL)
	handler := NewHandler(service)
	router := newCallbackRouter(handler)

	cookie, stored, _ := loginForCallback(t, handler)

	query := url.Values{}
	query.Set("code", "valid-auth-code")
	query.Set("state", stored.State)

	rec := callCallback(router, cookie, query)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}

	if body.Token == "" {
		t.Fatal("callback response is missing the Aevor JWT")
	}

	payload := decodedJWTClaims(t, body.Token)

	for _, sensitive := range []string{
		"fake-access-token",
		stored.State,
		stored.Verifier,
		"test-client-secret",
		"octocat@example.com",
		"The Octocat",
		"https://avatars.githubusercontent.com/u/583231",
	} {
		if strings.Contains(payload, sensitive) {
			t.Errorf("JWT payload contains %q", sensitive)
		}
	}
}
