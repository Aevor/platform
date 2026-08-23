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

// prFixture wires the real service/handler chain with a fake store and a mock
// GitHub API for pull-request sync tests. Users and selected-repository rows
// are registered per-test.
type prFixture struct {
	router *gin.Engine
	store  *fakeStore
	repo   *fakeListRepository
}

func newPRFixture(t *testing.T, githubHandler http.HandlerFunc) *prFixture {
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
		"/repositories/:id/pull-requests/sync",
		auth.RequireAuth(jwtManager),
		handler.SyncPullRequests,
	)

	return &prFixture{
		router: router,
		store:  store,
		repo:   repo,
	}
}

func (f *prFixture) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	manager := auth.NewJWTManager([]byte(testJWTSecret))

	token, err := manager.Issue(userID, time.Hour)

	if err != nil {
		t.Fatalf("could not issue a test JWT: %v", err)
	}

	return token
}

func (f *prFixture) addUser(id uuid.UUID, encryptedToken *string) {
	f.repo.users[id] = fixtureUser(id, encryptedToken)
}

func (f *prFixture) addSelected(id uuid.UUID, userID uuid.UUID) {
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
	prUserID     = uuid.MustParse("66666666-1111-1111-1111-111111111111")
	prForeignID  = uuid.MustParse("66666666-2222-2222-2222-222222222222")
	prSelected   = uuid.MustParse("77777777-0000-0000-0000-000000000001")
	prSelectedFg = uuid.MustParse("77777777-0000-0000-0000-000000000002")
	prSelectedNo = uuid.MustParse("77777777-0000-0000-0000-000000000009")
)

func prPageJSON(numbers ...int) string {
	parts := make([]string, 0, len(numbers))

	for _, n := range numbers {
		state := "open"

		if n%2 == 0 {
			state = "closed"
		}

		parts = append(parts, fmt.Sprintf(`{
			"id": %d,
			"number": %d,
			"title": "Pull request %d",
			"state": "%s",
			"user": {"login": "octocat"},
			"html_url": "https://github.com/octocat/hello-world/pull/%d",
			"head": {"ref": "feature-%d"},
			"base": {"ref": "main"},
			"draft": false,
			"merged": false,
			"created_at": "2026-01-02T03:04:05Z",
			"updated_at": "2026-02-03T04:05:06Z",
			"closed_at": null,
			"merged_at": null
		}`, 7000+n, n, n, state, n, n))
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func prBody(id int64, number int, title string, state string, merged bool) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"%s","state":"%s",
		"user":{"login":"octocat"},
		"html_url":"https://github.com/octocat/hello-world/pull/%d",
		"head":{"ref":"renamed-branch"},"base":{"ref":"develop"},
		"draft":false,"merged":%t,
		"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-09-09T09:09:09Z",
		"closed_at":"2026-08-08T08:08:08Z","merged_at":"2026-08-08T08:08:08Z"}`,
		id, number, title, state, number, merged)
}

func TestUpsertPullRequests_ColumnsMatchModelSchema(t *testing.T) {
	modelSchema, err := schema.Parse(&RepositoryPullRequest{}, &sync.Map{}, schema.NamingStrategy{})

	if err != nil {
		t.Fatalf("could not parse the RepositoryPullRequest model schema: %v", err)
	}

	columns := make(map[string]bool)

	for _, field := range modelSchema.Fields {
		if field.DBName != "" {
			columns[field.DBName] = true
		}
	}

	for _, column := range pullRequestUpsertConflictColumns {
		if !columns[column.Name] {
			t.Errorf(
				"pull request upsert conflict column %q is not mapped by the RepositoryPullRequest model; ON CONFLICT would reference a nonexistent column",
				column.Name,
			)
		}
	}

	for _, column := range pullRequestUpsertAssignmentColumns {
		if !columns[column] {
			t.Errorf(
				"pull request upsert assignment column %q is not mapped by the RepositoryPullRequest model; DO UPDATE SET would fail at runtime",
				column,
			)
		}
	}
}

func TestPRSync_UnauthenticatedRejected(t *testing.T) {
	var githubHits int32

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&githubHits, 1)
		w.Write([]byte(prPageJSON(1)))
	})

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	if atomic.LoadInt32(&githubHits) != 0 {
		t.Error("GitHub was contacted for an unauthenticated PR sync")
	}
}

func TestPRSync_MalformedSelectedRepositoryUUID(t *testing.T) {
	fixture := newPRFixture(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/repositories/not-a-uuid/pull-requests/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
		t.Errorf("status = %d body = %q, want 400 invalid_request", recorder.Code, recorder.Body.String())
	}
}

func TestPRSync_UnknownAndForeignUniform404(t *testing.T) {
	scenarios := []struct {
		name string
		id   uuid.UUID
	}{
		{"unknown selected repository", prSelectedNo},
		{"another user's selected repository", prSelectedFg},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var githubHits int32

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&githubHits, 1)
				w.Write([]byte(prPageJSON(1)))
			})

			fixture.addUser(prUserID, encryptedToken)
			fixture.addSelected(prSelected, prUserID)
			fixture.addSelected(prSelectedFg, prForeignID)

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+sc.id.String()+"/pull-requests/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

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

			if len(fixture.store.pullRequests) != 0 {
				t.Error("pull requests were persisted for an unowned repository")
			}
		})
	}
}

func TestPRSync_TokenProblemsMapped(t *testing.T) {
	t.Run("missing stored token -> 403 github_token_missing", func(t *testing.T) {
		fixture := newPRFixture(t, nil)

		fixture.addUser(prUserID, nil)
		fixture.addSelected(prSelected, prUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "github_token_missing") {
			t.Errorf("status = %d body = %q, want 403 github_token_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupt ciphertext -> 500 internal without crypto detail", func(t *testing.T) {
		fixture := newPRFixture(t, nil)

		fixture.addUser(prUserID, strPtrList("garbage-ciphertext"))
		fixture.addSelected(prSelected, prUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal") {
			t.Errorf("status = %d body = %q, want 500 internal", recorder.Code, recorder.Body.String())
		}
	})
}

func TestPRSync_GitHubFailureMappings(t *testing.T) {
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

			fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
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

			fixture.addUser(prUserID, encryptedToken)
			fixture.addSelected(prSelected, prUserID)

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != sc.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, sc.wantStatus)
			}

			if !strings.Contains(recorder.Body.String(), sc.wantError) {
				t.Errorf("body = %q, want %q", recorder.Body.String(), sc.wantError)
			}

			if len(fixture.store.pullRequests) != 0 {
				t.Errorf("%d pull requests were persisted despite a failed sync", len(fixture.store.pullRequests))
			}
		})
	}
}

func TestPRSync_SuccessPersistsMetadataAndReturnsCleanDTO(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth string

	encryptedToken, plaintext := encryptFixtureToken(t)

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(prPageJSON(1, 2, 3, 4)))
	})

	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if gotPath != "/repos/octocat/hello-world/pulls" {
		t.Errorf("GitHub path = %q; owner/name must come from OUR authoritative selection metadata", gotPath)
	}

	query, err := url.ParseQuery(gotRawQuery)

	if err != nil {
		t.Fatalf("could not parse query: %v", err)
	}

	for _, want := range [][2]string{
		{"state", "all"}, {"sort", "updated"}, {"direction", "desc"}, {"page", "1"},
	} {
		if got := query.Get(want[0]); got != want[1] {
			t.Errorf("query %s = %q, want %q", want[0], got, want[1])
		}
	}

	if gotAuth != "Bearer "+plaintext {
		t.Errorf("GitHub Authorization used %q, want the authenticated user's decrypted token", gotAuth)
	}

	var response SyncResult

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if response.RepositoryID != prSelected.String() || response.Synced != 4 {
		t.Errorf("response = %+v, want {repository_id:%s synced:4}", response, prSelected)
	}

	raw := recorder.Body.String()

	if strings.Contains(raw, plaintext) || strings.Contains(raw, string(*encryptedToken)) {
		t.Error("response leaks token material (plain or ciphertext)")
	}

	for key, stored := range fixture.store.pullRequests {
		if key.selectedRepositoryID != prSelected {
			t.Errorf("pull request persisted under wrong context %v", key)
		}

		if stored.Title == "" || stored.AuthorLogin == "" || stored.HTMLURL == "" ||
			stored.HeadRef == "" || stored.BaseRef == "" ||
			stored.GithubCreatedAt.IsZero() || stored.SyncedAt.IsZero() ||
			stored.Number <= 0 || stored.GithubPullRequestID <= 0 {
			t.Errorf("stored pull request = %+v, missing required metadata", stored)
		}
	}
}

func TestPRSync_PaginationBoundedToMaxPages(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	const totalFakePages = 30 // beyond syncMaxPages

	var requests int32

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))

		if page == int(n) && n < totalFakePages {
			w.Header().Set(
				"Link",
				fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next"`, r.Host, r.URL.Path, int(n)+1),
			)
		}

		w.Write([]byte(prPageJSON(int(n))))
	})

	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

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
		t.Errorf("synced = %d, want %d (one PR per fetched page)", response.Synced, syncMaxPages)
	}
}

func TestPRSync_RepeatedSyncDoesNotDuplicateAndRefreshesMetadata(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	syncCount := 0

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		syncCount++

		body := prPageJSON(1, 2)

		if syncCount >= 2 {
			body = "[" +
				prBody(7001, 1, "Pull request 1 (retitled)", "closed", true) + "," +
				strings.TrimSuffix(strings.TrimPrefix(prPageJSON(2), "["), "]") +
				"]"
		}

		w.Write([]byte(body))
	})

	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)

	sync := func() SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

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

	if len(fixture.store.pullRequests) != 2 {
		t.Fatalf("stored rows = %d, want exactly 2 after repeated sync (no duplicates)", len(fixture.store.pullRequests))
	}

	refreshed := fixture.store.pullRequests[pullRequestKey{prSelected, 7001}]

	if refreshed.Title != "Pull request 1 (retitled)" ||
		refreshed.State != "closed" || !refreshed.Merged ||
		refreshed.GithubMergedAt == nil || refreshed.GithubClosedAt == nil ||
		refreshed.HeadRef != "renamed-branch" || refreshed.BaseRef != "develop" {
		t.Errorf("re-sync did not refresh metadata: %+v", refreshed)
	}
}

func TestPRSync_CrossUserIsolationOnSameGithubRepository(t *testing.T) {
	const (
		tokenPlaintextA = "ghs_pr-isolation-user-a-token-aaaaaaaaaa"
		tokenPlaintextB = "ghs_pr-isolation-user-b-token-bbbbbbbbbb"
	)

	encryptedA, err := users.Encrypt(tokenPlaintextA, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt A: %v", err)
	}

	encryptedB, err := users.Encrypt(tokenPlaintextB, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt B: %v", err)
	}

	fixture := newPRFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Authorization") {
			case "Bearer " + tokenPlaintextA:
				w.Write([]byte(prPageJSON(1)))
			case "Bearer " + tokenPlaintextB:
				w.Write([]byte(prPageJSON(1, 2, 3, 4, 5)))
			default:
				t.Error("unexpected Authorization header reaching GitHub")
				w.WriteHeader(http.StatusUnauthorized)
			}
		},
	)

	fixture.addUser(prUserID, &encryptedA)
	fixture.addUser(prForeignID, &encryptedB)
	fixture.addSelected(prSelected, prUserID)
	fixture.addSelected(prSelectedFg, prForeignID)

	run := func(selected uuid.UUID, id uuid.UUID) SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selected.String()+"/pull-requests/sync", nil)
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

	resultA := run(prSelected, prUserID)
	resultB := run(prSelectedFg, prForeignID)

	if resultA.Synced != 1 || resultB.Synced != 5 {
		t.Errorf("per-user sync results = %d / %d, want 1 and 5", resultA.Synced, resultB.Synced)
	}

	countFor := func(context uuid.UUID) int {
		total := 0

		for key := range fixture.store.pullRequests {
			if key.selectedRepositoryID == context {
				total++
			}
		}

		return total
	}

	if countFor(prSelected) != 1 || countFor(prSelectedFg) != 5 {
		t.Errorf("context isolation broken: own=%d foreign=%d, want 1 and 5",
			countFor(prSelected), countFor(prSelectedFg))
	}
}

func TestPRSync_OverlappingPagesDeduplicated(t *testing.T) {
	// sort=updated pagination is not snapshot-stable: an item edited between
	// page fetches can be served on two pages of one sync. The batch handed
	// to the store must contain each GitHub PR exactly once, otherwise the
	// single multi-row ON CONFLICT INSERT fails with "cannot affect row a
	// second time".
	encryptedToken, _ := encryptFixtureToken(t)

	var requests int32

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		w.Header().Set(
			"Link",
			fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next"`, r.Host, r.URL.Path, int(n)+1),
		)

		// Every page serves THE SAME PR (as if it keeps shifting pages).
		w.Write([]byte(prPageJSON(7)))
	})

	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	var response SyncResult

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if response.Synced != 1 {
		t.Errorf("synced = %d, want 1 after collapsing duplicate GitHub IDs", response.Synced)
	}

	if len(fixture.store.pullRequests) != 1 {
		t.Errorf("stored rows = %d, want exactly 1", len(fixture.store.pullRequests))
	}
}

func TestPRSync_CancelledContextPersistsNothing(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	fixture := newPRFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(prPageJSON(1)))
	})

	fixture.addUser(prUserID, encryptedToken)
	fixture.addSelected(prSelected, prUserID)

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+prSelected.String()+"/pull-requests/sync", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token(t, prUserID))

	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, request)

	if len(fixture.store.pullRequests) != 0 {
		t.Error("pull requests were persisted despite a cancelled request context")
	}
}
