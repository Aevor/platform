package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/users"
)

func strPtr(s string) *string {
	return &s
}

type fakeMeRepository struct {
	users   map[uuid.UUID]*users.User
	byIDErr error
}

func newFakeMeRepository(userList ...*users.User) *fakeMeRepository {
	repo := &fakeMeRepository{
		users: make(map[uuid.UUID]*users.User),
	}

	for _, user := range userList {
		repo.users[user.ID] = user
	}

	return repo
}

func (f *fakeMeRepository) Create(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

func (f *fakeMeRepository) GetByID(id uuid.UUID) (*users.User, error) {
	if f.byIDErr != nil {
		return nil, f.byIDErr
	}

	user, ok := f.users[id]

	if !ok {
		return nil, gorm.ErrRecordNotFound
	}

	return user, nil
}

func (f *fakeMeRepository) GetByGitHubID(githubID int64) (*users.User, error) {
	for _, user := range f.users {
		if user.GithubID == githubID {
			return user, nil
		}
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeMeRepository) Update(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

func (f *fakeMeRepository) UpsertByGitHubID(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

func meUser(id uuid.UUID, githubID int64, username string) *users.User {
	return &users.User{
		ID:                id,
		GithubID:          githubID,
		Username:          username,
		DisplayName:       "Display " + username,
		Email:             username + "@example.com",
		AvatarURL:         "https://avatars.example.com/" + username,
		GitHubAccessToken: strPtr("ghp_FAKE_ACCESS_TOKEN"),
	}
}

func newMeService(repo users.Repository) *Service {
	userService := users.NewService(repo)
	jwtManager := NewJWTManager(testJWTSecret)

	return NewService(nil, userService, jwtManager, nil, nil)
}

func newMeRouter(service *Service) *gin.Engine {
	handler := NewHandler(service)

	router := gin.New()
	router.GET("/users/me", RequireAuth(service.jwtManager), handler.GetMe)

	return router
}

func callMe(router *gin.Engine, token string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/users/me", nil)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if mutate != nil {
		mutate(req)
	}

	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	return rec
}

func tamperJWT(token string) string {
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

func TestGetMe_ValidTokenReturnsAuthenticatedUser(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 583231, "octocat")
	service := newMeService(newFakeMeRepository(user))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["id"] != user.ID.String() {
		t.Errorf("id = %v, want %q (the verified JWT sub)", body["id"], user.ID)
	}

	if body["github_id"] != float64(user.GithubID) {
		t.Errorf("github_id = %v, want %d", body["github_id"], user.GithubID)
	}

	if body["username"] != user.Username {
		t.Errorf("username = %v, want %q", body["username"], user.Username)
	}

	if body["display_name"] != user.DisplayName {
		t.Errorf("display_name = %v, want %q", body["display_name"], user.DisplayName)
	}

	if body["email"] != user.Email {
		t.Errorf("email = %v, want %q", body["email"], user.Email)
	}

	if body["avatar_url"] != user.AvatarURL {
		t.Errorf("avatar_url = %v, want %q", body["avatar_url"], user.AvatarURL)
	}
}

func TestGetMe_MissingTokenRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	service := newMeService(newFakeMeRepository(meUser(uuid.New(), 1, "octocat")))
	router := newMeRouter(service)

	rec := callMe(router, "", nil)

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
}

func TestGetMe_InvalidTokenRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 1, "octocat")
	service := newMeService(newFakeMeRepository(user))
	router := newMeRouter(service)

	valid, err := service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, tamperJWT(valid), nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestGetMe_ExpiredTokenRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 1, "octocat")
	service := newMeService(newFakeMeRepository(user))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(user.ID, -time.Hour)

	if err != nil {
		t.Fatalf("Issue() expired token error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestGetMe_AnotherUsersTokenReturnsThatUsersData(t *testing.T) {
	gin.SetMode(gin.TestMode)

	alice := meUser(uuid.New(), 101, "alice")
	bob := meUser(uuid.New(), 202, "bob")
	service := newMeService(newFakeMeRepository(alice, bob))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(bob.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["id"] != bob.ID.String() {
		t.Errorf("id = %v, want Bob's UUID %q", body["id"], bob.ID)
	}

	if body["username"] != bob.Username {
		t.Errorf("username = %v, want %q", body["username"], bob.Username)
	}
}

func TestGetMe_QueryParamCannotOverrideIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	alice := meUser(uuid.New(), 101, "alice")
	bob := meUser(uuid.New(), 202, "bob")
	service := newMeService(newFakeMeRepository(alice, bob))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(alice.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, func(req *http.Request) {
		req.URL.RawQuery = "user_id=" + bob.ID.String()
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["id"] != alice.ID.String() {
		t.Errorf("id = %v, want Alice's UUID %q (query param ignored)", body["id"], alice.ID)
	}
}

func TestGetMe_RequestBodyCannotOverrideIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	alice := meUser(uuid.New(), 101, "alice")
	bob := meUser(uuid.New(), 202, "bob")
	service := newMeService(newFakeMeRepository(alice, bob))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(alice.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/users/me",
		bytes.NewBufferString(`{"user_id":"`+bob.ID.String()+`"}`),
	)

	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["id"] != alice.ID.String() {
		t.Errorf("id = %v, want Alice's UUID %q (request body ignored)", body["id"], alice.ID)
	}
}

func TestGetMe_ClientHeaderCannotOverrideIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	alice := meUser(uuid.New(), 101, "alice")
	bob := meUser(uuid.New(), 202, "bob")
	service := newMeService(newFakeMeRepository(alice, bob))
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(alice.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, func(req *http.Request) {
		req.Header.Set("X-User-Id", bob.ID.String())
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body["id"] != alice.ID.String() {
		t.Errorf("id = %v, want Alice's UUID %q (client header ignored)", body["id"], alice.ID)
	}
}

func TestGetMe_AuthenticatedUUIDDoesNotExist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	service := newMeService(newFakeMeRepository(meUser(uuid.New(), 1, "octocat")))
	router := newMeRouter(service)

	ghostID := uuid.New()

	token, err := service.jwtManager.Issue(ghostID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if body["error"] != "user_not_found" {
		t.Errorf("error = %q, want %q", body["error"], "user_not_found")
	}
}

func TestGetMe_RepositoryFailureReturnsSafeInternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 1, "octocat")
	repo := newFakeMeRepository(user)
	repo.byIDErr = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused (postgres)")

	service := newMeService(repo)
	router := newMeRouter(service)

	token, err := service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	var body map[string]string

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error body: %v", err)
	}

	if body["error"] != "internal" {
		t.Errorf("error = %q, want %q", body["error"], "internal")
	}

	for _, leaked := range []string{"dial", "127.0.0.1", "5432", "connection refused", "postgres"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("error response leaks database detail %q", leaked)
		}
	}
}

func TestGetMe_SecretsNeverReturned(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 583231, "octocat")
	service := newMeService(newFakeMeRepository(user))
	router := newMeRouter(service)

	state, err := GenerateState()

	if err != nil {
		t.Fatalf("GenerateState() error: %v", err)
	}

	verifier := oauth2.GenerateVerifier()

	token, err := service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	rec := callMe(router, token, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	wantKeys := []string{"id", "github_id", "username", "display_name", "email", "avatar_url"}

	for _, key := range wantKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("response is missing approved field %q", key)
		}
	}

	if len(body) != len(wantKeys) {
		t.Errorf("response has %d fields, want exactly %d", len(body), len(wantKeys))
	}

	for _, forbidden := range []string{
		"ghp_FAKE_ACCESS_TOKEN",
		"access_token",
		"github_access_token",
		string(testJWTSecret),
		state,
		verifier,
		"code_verifier",
		"state",
		"verifier",
		"secret",
		"created_at",
		"updated_at",
	} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("response contains forbidden material %q", forbidden)
		}
	}
}

func TestGetMe_HandlerRejectsRequestsWithoutMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 1, "octocat")
	service := newMeService(newFakeMeRepository(user))

	router := gin.New()
	router.GET("/users/me", NewHandler(service).GetMe)

	rec := callMe(router, "", nil)

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
}

func TestGetMe_DoesNotConflictWithUsersIDRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	user := meUser(uuid.New(), 583231, "octocat")
	service := newMeService(newFakeMeRepository(user))
	handler := NewHandler(service)

	router := gin.New()
	router.GET("/users/me", RequireAuth(service.jwtManager), handler.GetMe)
	router.GET("/users/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"param": c.Param("id")})
	})

	token, err := service.jwtManager.Issue(user.ID, defaultTTL)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	meRec := callMe(router, token, nil)

	if meRec.Code != http.StatusOK {
		t.Fatalf("/users/me status = %d, want %d (body %s)", meRec.Code, http.StatusOK, meRec.Body.String())
	}

	var meBody map[string]any

	if err := json.Unmarshal(meRec.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if meBody["id"] != user.ID.String() {
		t.Errorf("/users/me resolved to wrong handler: id = %v, want %q", meBody["id"], user.ID)
	}

	idReq := httptest.NewRequest(http.MethodGet, "/users/"+user.ID.String(), nil)
	idRec := httptest.NewRecorder()

	router.ServeHTTP(idRec, idReq)

	var idBody map[string]string

	if err := json.Unmarshal(idRec.Body.Bytes(), &idBody); err != nil {
		t.Fatalf("invalid JSON response from /users/:id: %v", err)
	}

	if idBody["param"] != user.ID.String() {
		t.Errorf("/users/:id param = %q, want %q", idBody["param"], user.ID)
	}

	unauthMe := callMe(router, "", nil)

	if unauthMe.Code != http.StatusUnauthorized {
		t.Fatalf("/users/me without token status = %d, want %d (body %s)", unauthMe.Code, http.StatusUnauthorized, unauthMe.Body.String())
	}
}
