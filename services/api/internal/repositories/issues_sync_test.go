package repositories

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm/schema"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
)

// syncFixture wires the real service/handler chain with a fake store and a
// mock GitHub API. Users and selected-repository rows are registered per-test.
type syncFixture struct {
	router *gin.Engine
	store  *fakeStore
	repo   *fakeListRepository
}

func newSyncFixture(t *testing.T, githubHandler http.HandlerFunc) *syncFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	repo := &fakeListRepository{users: make(map[uuid.UUID]*users.User)}

	var mock *httptest.Server

	if githubHandler != nil {
		mock = httptest.NewServer(githubHandler)
	} else {
		mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[]`))
		}))
	}

	t.Cleanup(mock.Close)

	userService := users.NewService(repo)

	client := github.NewClient(nil, github.WithBaseURL(mock.URL))

	store := &fakeStore{
		rows:         make(map[uuid.UUID]SelectedRepository),
		issues:       make(map[issueKey]RepositoryIssue),
		pullRequests: make(map[pullRequestKey]RepositoryPullRequest),
	}

	service := NewService(userService, client, store, testEncryptionKey())

	jwtManager := auth.NewJWTManager([]byte(testJWTSecret))

	handler := NewHandler(service)

	router := gin.New()
	router.POST(
		"/repositories/:id/issues/sync",
		auth.RequireAuth(jwtManager),
		handler.SyncIssues,
	)

	return &syncFixture{
		router: router,
		store:  store,
		repo:   repo,
	}
}

func (f *syncFixture) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	manager := auth.NewJWTManager([]byte(testJWTSecret))

	token, err := manager.Issue(userID, time.Hour)

	if err != nil {
		t.Fatalf("could not issue a test JWT: %v", err)
	}

	return token
}

// addUser registers a user; encryptedToken may be nil (missing token).
func (f *syncFixture) addUser(id uuid.UUID, encryptedToken *string) {
	f.repo.users[id] = fixtureUser(id, encryptedToken)
}

// addSelected registers a selected-repository context pointing at
// octocat/hello-world (the owner/name pair our metadata supplies to GitHub).
func (f *syncFixture) addSelected(id uuid.UUID, userID uuid.UUID) {
	f.store.rows[id] = SelectedRepository{
		ID:                 id,
		UserID:             userID,
		GithubRepositoryID: 1296269,
		Name:               "hello-world",
		FullName:           "octocat/hello-world",
		OwnerLogin:         "octocat",
	}
}

var (
	syncUserID     = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	foreignUserID  = uuid.MustParse("55555555-5555-5555-5555-555555555555")
	selectedIDOwn  = uuid.MustParse("99999999-0000-0000-0000-000000000001")
	selectedIDFgn  = uuid.MustParse("99999999-0000-0000-0000-000000000002")
	selectedIDNone = uuid.MustParse("99999999-0000-0000-0000-000000000009")
)

func issuePageJSON(numbers ...int) string {
	parts := make([]string, 0, len(numbers))

	for _, n := range numbers {
		state := "open"

		if n%2 == 0 {
			state = "closed"
		}

		closed := ""

		if state == "closed" {
			closed = `,"closed_at":"2026-03-04T05:06:07Z"`
		}

		parts = append(parts, fmt.Sprintf(`{
			"id": %d,
			"number": %d,
			"title": "Issue %d",
			"state": "%s",
			"user": {"login": "octocat"},
			"html_url": "https://github.com/octocat/hello-world/issues/%d",
			"created_at": "2026-01-02T03:04:05Z",
			"updated_at": "2026-02-03T04:05:06Z"%s
		}`, 9000+n, n, n, state, n, closed))
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func issueBody(id int64, number int, title string, state string, closedAt string) string {
	closed := ""

	if closedAt != "" {
		closed = `,"closed_at":"` + closedAt + `"`
	}

	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"%s","state":"%s",
		"user":{"login":"octocat"},
		"html_url":"https://github.com/octocat/hello-world/issues/%d",
		"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-02-03T04:05:06Z"%s}`,
		id, number, title, state, number, closed)
}

func TestUpsertIssues_ColumnsMatchModelSchema(t *testing.T) {
	modelSchema, err := schema.Parse(&RepositoryIssue{}, &sync.Map{}, schema.NamingStrategy{})

	if err != nil {
		t.Fatalf("could not parse the RepositoryIssue model schema: %v", err)
	}

	columns := make(map[string]bool)

	for _, field := range modelSchema.Fields {
		if field.DBName != "" {
			columns[field.DBName] = true
		}
	}

	for _, column := range issueUpsertConflictColumns {
		if !columns[column.Name] {
			t.Errorf(
				"issue upsert conflict column %q is not mapped by the RepositoryIssue model; ON CONFLICT would reference a nonexistent column",
				column.Name,
			)
		}
	}

	for _, column := range issueUpsertAssignmentColumns {
		if !columns[column] {
			t.Errorf(
				"issue upsert assignment column %q is not mapped by the RepositoryIssue model; DO UPDATE SET would fail at runtime",
				column,
			)
		}
	}
}

func TestSync_UnauthenticatedRejected(t *testing.T) {
	var githubHits int32

	fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&githubHits, 1)
		w.Write([]byte(issuePageJSON(1)))
	})

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	if atomic.LoadInt32(&githubHits) != 0 {
		t.Error("GitHub was contacted for an unauthenticated sync")
	}
}

func TestSync_MalformedSelectedRepositoryUUID(t *testing.T) {
	fixture := newSyncFixture(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/repositories/not-a-uuid/issues/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
		t.Errorf("status = %d body = %q, want 400 invalid_request", recorder.Code, recorder.Body.String())
	}
}

func TestSync_SelectedRepositoryNotFoundOrForeignIsUniform404(t *testing.T) {
	scenarios := []struct {
		name string
		id   uuid.UUID
	}{
		{"unknown selected repository", selectedIDNone},
		{"another user's selected repository", selectedIDFgn},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var githubHits int32

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&githubHits, 1)
				w.Write([]byte(issuePageJSON(1)))
			})

			fixture.addUser(syncUserID, encryptedToken)
			fixture.addSelected(selectedIDOwn, syncUserID)
			fixture.addSelected(selectedIDFgn, foreignUserID) // belongs to someone else

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+sc.id.String()+"/issues/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}

			if !strings.Contains(recorder.Body.String(), "repository_not_found") {
				t.Errorf("body = %q, want repository_not_found (foreign existence never revealed)", recorder.Body.String())
			}

			if atomic.LoadInt32(&githubHits) != 0 {
				t.Error("GitHub was contacted for a repository the user does not own")
			}

			if len(fixture.store.issues) != 0 {
				t.Error("issues were persisted for an unowned repository")
			}
		})
	}
}

func TestSync_TokenProblemsMapped(t *testing.T) {
	t.Run("missing stored token -> 403 github_token_missing", func(t *testing.T) {
		fixture := newSyncFixture(t, nil)

		fixture.addUser(syncUserID, nil)
		fixture.addSelected(selectedIDOwn, syncUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "github_token_missing") {
			t.Errorf("status = %d body = %q, want 403 github_token_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupt ciphertext -> 500 internal without crypto detail", func(t *testing.T) {
		fixture := newSyncFixture(t, nil)

		fixture.addUser(syncUserID, strPtrList("garbage-ciphertext"))
		fixture.addSelected(selectedIDOwn, syncUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal") {
			t.Errorf("status = %d body = %q, want 500 internal", recorder.Code, recorder.Body.String())
		}
	})
}

func TestSync_GitHubFailureMappings(t *testing.T) {
	scenarios := []struct {
		name         string
		githubStatus int
		wantStatus   int
		wantError    string
	}{
		{"token rejected", http.StatusUnauthorized, http.StatusUnauthorized, "github_token_invalid"},
		{"rate limited", http.StatusTooManyRequests, http.StatusTooManyRequests, "github_rate_limited"},
		{"github down", http.StatusInternalServerError, http.StatusInternalServerError, "github_unavailable"},
		{"repository renamed or deleted since selection", http.StatusNotFound, http.StatusNotFound, "repository_not_found"},
		{"malformed payload", http.StatusOK, http.StatusInternalServerError, "github_unavailable"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.githubStatus)

				switch sc.githubStatus {
				case http.StatusOK:
					w.Write([]byte(`{not-json`))
				case http.StatusNotFound:
					// empty body, like GitHub's real 404s
				default:
					w.Write([]byte(`{"message":"nope"}`))
				}
			})

			fixture.addUser(syncUserID, encryptedToken)
			fixture.addSelected(selectedIDOwn, syncUserID)

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != sc.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, sc.wantStatus)
			}

			if !strings.Contains(recorder.Body.String(), sc.wantError) {
				t.Errorf("body = %q, want %q", recorder.Body.String(), sc.wantError)
			}

			if len(fixture.store.issues) != 0 {
				t.Errorf("%d issues were persisted despite a failed sync", len(fixture.store.issues))
			}
		})
	}
}

func TestSync_SuccessPersistsMetadataAndReturnsCleanDTO(t *testing.T) {
	var gotPath, gotAuth string

	encryptedToken, plaintext := encryptFixtureToken(t)

	fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(issuePageJSON(1, 2, 3)))
	})

	fixture.addUser(syncUserID, encryptedToken)
	fixture.addSelected(selectedIDOwn, syncUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if gotAuth != "Bearer "+plaintext {
		t.Errorf("GitHub Authorization used %q, want the authenticated user's decrypted token", gotAuth)
	}

	gotURL, err := url.ParseRequestURI("http://mock" + gotPath)

	if err != nil {
		t.Fatalf("could not parse the GitHub request URL: %v", err)
	}

	if gotURL.Path != "/repos/octocat/hello-world/issues" {
		t.Errorf("GitHub path = %q; owner/name must come from OUR authoritative selection metadata", gotURL.Path)
	}

	query := gotURL.Query()

	for _, want := range []struct{ key, value string }{
		{"state", "all"},
		{"sort", "updated"},
		{"direction", "desc"},
		{"page", "1"},
		{"per_page", fmt.Sprint(syncPerPage)},
	} {
		if got := query.Get(want.key); got != want.value {
			t.Errorf("GitHub query %s = %q, want %q", want.key, got, want.value)
		}
	}

	var response struct {
		RepositoryID string `json:"repository_id"`
		Synced       int    `json:"synced"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if response.RepositoryID != selectedIDOwn.String() || response.Synced != 3 {
		t.Errorf("response = %+v, want {repository_id:%s synced:3}", response, selectedIDOwn)
	}

	if extraKeys(recorder.Body.String()) {
		t.Error("response carries fields beyond repository_id/synced")
	}

	if strings.Contains(recorder.Body.String(), plaintext) || strings.Contains(recorder.Body.String(), string(*encryptedToken)) {
		t.Error("response leaks token material (plain or ciphertext)")
	}

	for key, stored := range fixture.store.issues {
		if key.selectedRepositoryID != selectedIDOwn {
			t.Errorf("issue persisted under wrong context %v", key)
		}

		if stored.Title == "" || stored.AuthorLogin == "" || stored.HTMLURL == "" ||
			stored.GithubCreatedAt.IsZero() || stored.SyncedAt.IsZero() ||
			(stored.State != "open" && stored.State != "closed") ||
			stored.Number <= 0 || stored.GithubIssueID <= 0 {
			t.Errorf("stored issue = %+v, missing required metadata", stored)
		}
	}
}

func extraKeys(body string) bool {
	var payload map[string]json.RawMessage

	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return true
	}

	for key := range payload {
		if key != "repository_id" && key != "synced" {
			return true
		}
	}

	return false
}

func TestSync_PaginationBoundedToMaxPages(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	const totalFakePages = 25 // far beyond syncMaxPages

	var requests int32

	fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))

		if page == int(n) && n < totalFakePages {
			w.Header().Set(
				"Link",
				fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next"`, r.Host, r.URL.Path, int(n)+1),
			)
		}

		w.Write([]byte(issuePageJSON(int(n))))
	})

	fixture.addUser(syncUserID, encryptedToken)
	fixture.addSelected(selectedIDOwn, syncUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	if got := atomic.LoadInt32(&requests); got != int32(syncMaxPages) {
		t.Errorf("GitHub requests = %d, want exactly %d (bounded sync)", got, syncMaxPages)
	}

	var response SyncResult

	_ = json.Unmarshal(recorder.Body.Bytes(), &response)

	if response.Synced != syncMaxPages {
		t.Errorf("synced = %d, want %d (one issue per fetched page)", response.Synced, syncMaxPages)
	}
}

func TestSync_RepeatedSyncDoesNotDuplicateAndRefreshesMetadata(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	syncCount := 0

	fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		syncCount++

		body := issuePageJSON(1, 2)

		if syncCount >= 2 {
			body = "[" +
				issueBody(9001, 1, "Issue 1 (renamed)", "closed", "2026-08-08T08:08:08Z") + "," +
				strings.TrimSuffix(strings.TrimPrefix(issuePageJSON(2), "["), "]") +
				"]"
		}

		w.Write([]byte(body))
	})

	fixture.addUser(syncUserID, encryptedToken)
	fixture.addSelected(selectedIDOwn, syncUserID)

	sync := func() SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}

		var result SyncResult

		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode: %v", err)
		}

		return result
	}

	first := sync()
	second := sync()

	if first.Synced != 2 || second.Synced != 2 {
		t.Fatalf("sync counts = %d/%d, want 2/2", first.Synced, second.Synced)
	}

	if len(fixture.store.issues) != 2 {
		t.Fatalf("stored rows = %d, want exactly 2 after repeated sync (no duplicates)", len(fixture.store.issues))
	}

	renamed := fixture.store.issues[issueKey{selectedIDOwn, 9001}]

	if renamed.Title != "Issue 1 (renamed)" || renamed.State != "closed" || renamed.GithubClosedAt == nil {
		t.Errorf("re-sync did not refresh metadata: %+v", renamed)
	}
}

func TestSync_CrossUserIsolationOnSameGithubRepository(t *testing.T) {
	const (
		tokenPlaintextA = "ghs_isolation-user-a-token-aaaaaaaaaa"
		tokenPlaintextB = "ghs_isolation-user-b-token-bbbbbbbbbb"
	)

	encryptedA, err := users.Encrypt(tokenPlaintextA, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt A: %v", err)
	}

	encryptedB, err := users.Encrypt(tokenPlaintextB, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt B: %v", err)
	}

	fixture := newSyncFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			// Both contexts point at the same GitHub repository; each sync
			// must use THAT user's own decrypted token.
			switch r.Header.Get("Authorization") {
			case "Bearer " + tokenPlaintextA:
				w.Write([]byte(issuePageJSON(1)))
			case "Bearer " + tokenPlaintextB:
				w.Write([]byte(issuePageJSON(1, 2, 3, 4)))
			default:
				t.Errorf("unexpected Authorization header reaching GitHub")
				w.WriteHeader(http.StatusUnauthorized)
			}
		},
	)

	fixture.addUser(syncUserID, &encryptedA)
	fixture.addUser(foreignUserID, &encryptedB)
	fixture.addSelected(selectedIDOwn, syncUserID)
	fixture.addSelected(selectedIDFgn, foreignUserID)

	run := func(selected uuid.UUID, id uuid.UUID) SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selected.String()+"/issues/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, id))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}

		var result SyncResult

		_ = json.Unmarshal(recorder.Body.Bytes(), &result)

		return result
	}

	resultA := run(selectedIDOwn, syncUserID)
	resultB := run(selectedIDFgn, foreignUserID)

	if resultA.Synced != 1 || resultB.Synced != 4 {
		t.Errorf("per-user sync results = %d / %d, want 1 and 4", resultA.Synced, resultB.Synced)
	}

	countFor := func(context uuid.UUID) int {
		total := 0

		for key := range fixture.store.issues {
			if key.selectedRepositoryID == context {
				total++
			}
		}

		return total
	}

	if countFor(selectedIDOwn) != 1 || countFor(selectedIDFgn) != 4 {
		t.Errorf("context isolation broken: own=%d foreign=%d, want 1 and 4",
			countFor(selectedIDOwn), countFor(selectedIDFgn))
	}
}

func TestSync_GitHubContextCancellationPropagates(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	fixture := newSyncFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(issuePageJSON(1)))
	})

	fixture.addUser(syncUserID, encryptedToken)
	fixture.addSelected(selectedIDOwn, syncUserID)

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedIDOwn.String()+"/issues/sync", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token(t, syncUserID))

	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, request)

	if len(fixture.store.issues) != 0 {
		t.Error("issues were persisted despite a cancelled request context")
	}
}
