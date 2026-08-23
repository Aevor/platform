package repositories

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

const (
	testJWTSecret        = "aevor-repositories-test-jwt-signing-secret!"
	testPlaintextToken   = "ghs_repositories-test-token-do-not-leak"
	repositoriesResponse = `[
		{
			"id": 1296269,
			"name": "hello-world",
			"full_name": "octocat/hello-world",
			"private": false,
			"description": "My first repository",
			"default_branch": "main",
			"owner": {"login": "octocat"},
			"html_url": "https://github.com/octocat/hello-world",
			"clone_url": "https://github.com/octocat/hello-world.git",
			"url": "https://api.github.com/repos/octocat/hello-world"
		}
	]`
)

func testEncryptionKey() []byte {
	key := make([]byte, 32)

	for i := range key {
		key[i] = byte('A' + i%26)
	}

	return key
}

type fakeListRepository struct {
	users map[uuid.UUID]*users.User
	err   error
}

func (f *fakeListRepository) Create(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

func (f *fakeListRepository) GetByID(id uuid.UUID) (*users.User, error) {
	if f.err != nil {
		return nil, f.err
	}

	user, ok := f.users[id]

	if !ok {
		return nil, gorm.ErrRecordNotFound
	}

	return user, nil
}

func (f *fakeListRepository) GetByGitHubID(githubID int64) (*users.User, error) {
	for _, user := range f.users {
		if user.GithubID == githubID {
			return user, nil
		}
	}

	return nil, gorm.ErrRecordNotFound
}

func (f *fakeListRepository) Update(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

func (f *fakeListRepository) UpsertByGitHubID(user *users.User) error {
	f.users[user.ID] = user

	return nil
}

// listFixture bundles the full real dependency chain with a fake persistence
// layer and a mock GitHub API: fake users.Repository -> users.Service ->
// repositories.Service -> github.Client -> httptest server.
type listFixture struct {
	router     *gin.Engine
	githubMock *httptest.Server
	repo       *fakeListRepository
	jwtManager *auth.JWTManager
}

func newListFixture(
	t *testing.T,
	githubHandler http.HandlerFunc,
	userRows ...*users.User,
) *listFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	repo := &fakeListRepository{users: make(map[uuid.UUID]*users.User)}

	for _, user := range userRows {
		repo.users[user.ID] = user
	}

	var githubMock *httptest.Server

	if githubHandler != nil {
		githubMock = httptest.NewServer(githubHandler)
	} else {
		githubMock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`))
		}))
	}

	t.Cleanup(githubMock.Close)

	userService := users.NewService(repo)

	client := github.NewClient(nil, github.WithBaseURL(githubMock.URL))

	service := NewService(userService, client, testEncryptionKey())

	jwtManager := auth.NewJWTManager([]byte(testJWTSecret))

	handler := NewHandler(service)

	router := gin.New()
	router.GET(
		"/repositories",
		auth.RequireAuth(jwtManager),
		handler.List,
	)

	return &listFixture{
		router:     router,
		githubMock: githubMock,
		repo:       repo,
		jwtManager: jwtManager,
	}
}

func (f *listFixture) tokenFor(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	token, err := f.jwtManager.Issue(userID, time.Hour)

	if err != nil {
		t.Fatalf("Issue() error: %v", err)
	}

	return token
}

func (f *listFixture) call(token string, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()

	f.router.ServeHTTP(recorder, req)

	return recorder
}

func decodeListResponse(t *testing.T, body []byte) ListResponse {
	t.Helper()

	var response ListResponse

	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("could not decode response body: %v", err)
	}

	return response
}

func fixtureUser(id uuid.UUID, encryptedToken *string) *users.User {
	return &users.User{
		ID:                id,
		GithubID:          583231,
		Username:          "octocat",
		DisplayName:       "The Octocat",
		Email:             "octocat@example.com",
		AvatarURL:         "https://avatars.githubusercontent.com/u/583231?v=4",
		GitHubAccessToken: encryptedToken,
	}
}

func TestList_UnauthenticatedRejected(t *testing.T) {
	var githubHit bool

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		githubHit = true
		w.Write([]byte(repositoriesResponse))
	}, fixtureUser(uuid.New(), nil))

	recorder := fixture.call("", "/repositories")

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	if strings.Contains(recorder.Body.String(), "repositories") {
		t.Errorf("body = %q, want only the uniform unauthorized error", recorder.Body.String())
	}

	if githubHit {
		t.Error("GitHub API was contacted for an unauthenticated request")
	}
}

func TestList_SuccessReturnsMappedRepositories(t *testing.T) {
	userID := uuid.New()

	encryptedToken, err := users.Encrypt(testPlaintextToken, testEncryptionKey())

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(repositoriesResponse))
	}, fixtureUser(userID, &encryptedToken))

	recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	response := decodeListResponse(t, recorder.Body.Bytes())

	if len(response.Repositories) != 1 {
		t.Fatalf("repository count = %d, want 1", len(response.Repositories))
	}

	repository := response.Repositories[0]

	want := RepositoryResponse{
		ID:            1296269,
		Name:          "hello-world",
		FullName:      "octocat/hello-world",
		Description:   "My first repository",
		Private:       false,
		DefaultBranch: "main",
		OwnerLogin:    "octocat",
		HTMLURL:       "https://github.com/octocat/hello-world",
		CloneURL:      "https://github.com/octocat/hello-world.git",
		APIURL:        "https://api.github.com/repos/octocat/hello-world",
	}

	if repository != want {
		t.Errorf("repository = %+v, want %+v", repository, want)
	}

	if response.Page != defaultPage || response.PerPage != defaultPerPage || response.HasMore {
		t.Errorf("pagination echo = {page:%d per_page:%d has_more:%v}, want defaults without next page",
			response.Page, response.PerPage, response.HasMore)
	}
}

func TestList_TokenNeverPresentInResponse(t *testing.T) {
	userID := uuid.New()

	encryptedToken, err := users.Encrypt(testPlaintextToken, testEncryptionKey())

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(repositoriesResponse))
	}, fixtureUser(userID, &encryptedToken))

	recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

	body := recorder.Body.String()

	if strings.Contains(body, testPlaintextToken) {
		t.Error("response body contains the decrypted GitHub access token")
	}

	if strings.Contains(body, encryptedToken) {
		t.Error("response body contains the stored encrypted GitHub access token")
	}
}

func TestList_PaginationPassedThroughAndClamped(t *testing.T) {
	var gotQuery url.Values

	userID := uuid.New()

	encryptedToken, _ := users.Encrypt(testPlaintextToken, testEncryptionKey())

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Link", `</user/repos?page=3>; rel="next"`)
		w.Write([]byte(repositoriesResponse))
	}, fixtureUser(userID, &encryptedToken))

	recorder := fixture.call(
		fixture.tokenFor(t, userID),
		"/repositories?page=2&per_page=500",
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	if got := gotQuery.Get("page"); got != "2" {
		t.Errorf("forwarded page = %q, want 2", got)
	}

	if got := gotQuery.Get("per_page"); got != "100" {
		t.Errorf("forwarded per_page = %q, want clamped to 100", got)
	}

	response := decodeListResponse(t, recorder.Body.Bytes())

	if response.Page != 2 || response.PerPage != maxPerPage {
		t.Errorf("pagination echo = {page:%d per_page:%d}, want {2 100}", response.Page, response.PerPage)
	}

	if !response.HasMore {
		t.Error("has_more = false, want true when GitHub advertises a next page")
	}
}

func TestList_EmptyResultIsJSONEmptyArray(t *testing.T) {
	userID := uuid.New()

	encryptedToken, _ := users.Encrypt(testPlaintextToken, testEncryptionKey())

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}, fixtureUser(userID, &encryptedToken))

	recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), `"repositories":[]`) {
		t.Errorf("body = %q, want an empty JSON array (not null)", recorder.Body.String())
	}
}

func TestList_InvalidPaginationRejected(t *testing.T) {
	scenarios := []string{
		"/repositories?page=abc",
		"/repositories?page=0",
		"/repositories?page=-3",
		"/repositories?per_page=abc",
		"/repositories?per_page=0",
	}

	for _, target := range scenarios {
		t.Run(target, func(t *testing.T) {
			userID := uuid.New()

			encryptedToken, _ := users.Encrypt(testPlaintextToken, testEncryptionKey())

			fixture := newListFixture(t, nil, fixtureUser(userID, &encryptedToken))

			recorder := fixture.call(fixture.tokenFor(t, userID), target)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", recorder.Code)
			}

			if !strings.Contains(recorder.Body.String(), "invalid_pagination") {
				t.Errorf("body = %q, want invalid_pagination", recorder.Body.String())
			}
		})
	}
}

func TestList_UserNotFoundMapped(t *testing.T) {
	fixture := newListFixture(t, nil)

	recorder := fixture.call(fixture.tokenFor(t, uuid.New()), "/repositories")

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "user_not_found") {
		t.Errorf("body = %q, want user_not_found", recorder.Body.String())
	}
}

func TestList_MissingStoredTokenMapped(t *testing.T) {
	userID := uuid.New()

	fixture := newListFixture(t, nil, fixtureUser(userID, nil))

	recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "github_token_missing") {
		t.Errorf("body = %q, want github_token_missing", recorder.Body.String())
	}
}

func TestList_DecryptionFailureIsInternal(t *testing.T) {
	userID := uuid.New()

	fixture := newListFixture(t, nil, fixtureUser(userID, strPtrList("definitely-not-a-valid-ciphertext")))

	recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "internal") {
		t.Errorf("body = %q, want internal (no crypto detail leaked)", recorder.Body.String())
	}

	if strings.Contains(recorder.Body.String(), "ciphertext") {
		t.Errorf("body = %q, must not leak internal crypto vocabulary", recorder.Body.String())
	}
}

func TestList_GitHubFailuresMapped(t *testing.T) {
	scenarios := []struct {
		name         string
		githubStatus int
		wantStatus   int
		wantError    string
	}{
		{"token rejected by GitHub", http.StatusUnauthorized, http.StatusUnauthorized, "github_token_invalid"},
		{"rate limited", http.StatusTooManyRequests, http.StatusTooManyRequests, "github_rate_limited"},
		{"github down", http.StatusInternalServerError, http.StatusInternalServerError, "github_unavailable"},
		{"malformed payload", http.StatusOK, http.StatusInternalServerError, "github_unavailable"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			userID := uuid.New()

			encryptedToken, _ := users.Encrypt(testPlaintextToken, testEncryptionKey())

			fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.githubStatus)

				if sc.githubStatus == http.StatusOK {
					w.Write([]byte(`{not-json`))
				} else {
					w.Write([]byte(`{"message":"nope"}`))
				}
			}, fixtureUser(userID, &encryptedToken))

			recorder := fixture.call(fixture.tokenFor(t, userID), "/repositories")

			if recorder.Code != sc.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, sc.wantStatus)
			}

			if !strings.Contains(recorder.Body.String(), sc.wantError) {
				t.Errorf("body = %q, want %q", recorder.Body.String(), sc.wantError)
			}

			if strings.Contains(recorder.Body.String(), testPlaintextToken) {
				t.Error("error response contains the GitHub access token")
			}
		})
	}
}

// TestList_IdentityComesOnlyFromJWT proves a query-parameter user_id can never
// select another user's credentials.
func TestList_IdentityComesOnlyFromJWT(t *testing.T) {
	userA := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	userB := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	tokenA, _ := users.Encrypt("ghs_user-a-token", testEncryptionKey())
	tokenB, _ := users.Encrypt("ghs_user-b-token", testEncryptionKey())

	var gotAuth string

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`[]`))
	},
		fixtureUser(userA, &tokenA),
		fixtureUser(userB, &tokenB),
	)

	recorder := fixture.call(
		fixture.tokenFor(t, userA),
		"/repositories?user_id="+userB.String(),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	if gotAuth != "Bearer ghs_user-a-token" {
		t.Errorf("GitHub Authorization = %q, want user A's token (identity from JWT, not query)", gotAuth)
	}
}

func TestList_UsesRequestContext(t *testing.T) {
	userID := uuid.New()

	encryptedToken, _ := users.Encrypt(testPlaintextToken, testEncryptionKey())

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Context() == context.Background() {
			t.Error("request reached GitHub without the inbound request context")
		}

		w.Write([]byte(`[]`))
	}, fixtureUser(userID, &encryptedToken))

	fixture.call(fixture.tokenFor(t, userID), "/repositories")
}

func strPtrList(s string) *string {
	return &s
}
