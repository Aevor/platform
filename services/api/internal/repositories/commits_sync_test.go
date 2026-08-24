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

// commitFixture wires the real service/handler chain with a fake store and a
// mock GitHub API for commit sync tests.
type commitFixture struct {
	router *gin.Engine
	store  *fakeStore
	repo   *fakeListRepository
}

func newCommitFixture(t *testing.T, githubHandler http.HandlerFunc) *commitFixture {
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
		commits:      make(map[commitKey]RepositoryCommit),
	}

	service := newTestService(t, userService, client, store)

	jwtManager := auth.NewJWTManager([]byte(testJWTSecret))

	handler := NewHandler(service)

	router := gin.New()
	router.POST(
		"/repositories/:id/commits/sync",
		auth.RequireAuth(jwtManager),
		handler.SyncCommits,
	)

	return &commitFixture{
		router: router,
		store:  store,
		repo:   repo,
	}
}

func (f *commitFixture) token(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	manager := auth.NewJWTManager([]byte(testJWTSecret))

	token, err := manager.Issue(userID, time.Hour)

	if err != nil {
		t.Fatalf("could not issue a test JWT: %v", err)
	}

	return token
}

func (f *commitFixture) addUser(id uuid.UUID, encryptedToken *string) {
	f.repo.users[id] = fixtureUser(id, encryptedToken)
}

func (f *commitFixture) addSelected(id uuid.UUID, userID uuid.UUID) {
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
	cmtUserID     = uuid.MustParse("88888888-1111-1111-1111-111111111111")
	cmtForeignID  = uuid.MustParse("88888888-2222-2222-2222-222222222222")
	cmtSelected   = uuid.MustParse("99999999-1000-0000-0000-000000000001")
	cmtSelectedFg = uuid.MustParse("99999999-1000-0000-0000-000000000002")
	cmtSelectedNo = uuid.MustParse("99999999-1000-0000-0000-000000000009")

	commitSHAFixA = "3a1f9c4b7d2e8f6051a4b3c2d1e0f9876543210a"
	commitSHAFixB = "bb29c8d7e6f504132a1b0c9d8e7f6a5b4c3d2e1f"
)

func commitPageJSON(shas ...string) string {
	parts := make([]string, 0, len(shas))

	for i, sha := range shas {
		var account string

		if i%2 == 0 {
			account = `"author": {"login": "octocat"},`
		} else {
			account = `"author": null,`
		}

		parts = append(parts, fmt.Sprintf(`{
			"sha": "%s",
			%s
			"html_url": "https://github.com/octocat/hello-world/commit/%s",
			"commit": {
				"message": "Commit %s",
				"author": {"name": "Alice Author", "email": "alice@example.com", "date": "2026-01-02T03:04:05Z"},
				"committer": {"name": "Committer One", "email": "c@example.com", "date": "2026-02-03T04:05:06Z"}
			}
		}`, sha, strings.TrimSpace(account), sha[:12], sha[:7]))
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func TestUpsertCommits_ColumnsMatchModelSchema(t *testing.T) {
	modelSchema, err := schema.Parse(&RepositoryCommit{}, &sync.Map{}, schema.NamingStrategy{})

	if err != nil {
		t.Fatalf("could not parse the RepositoryCommit model schema: %v", err)
	}

	columns := make(map[string]bool)

	for _, field := range modelSchema.Fields {
		if field.DBName != "" {
			columns[field.DBName] = true
		}
	}

	for _, column := range commitUpsertConflictColumns {
		if !columns[column.Name] {
			t.Errorf(
				"commit upsert conflict column %q is not mapped by the RepositoryCommit model; ON CONFLICT would reference a nonexistent column",
				column.Name,
			)
		}
	}

	for _, column := range commitUpsertAssignmentColumns {
		if !columns[column] {
			t.Errorf(
				"commit upsert assignment column %q is not mapped by the RepositoryCommit model; DO UPDATE SET would fail at runtime",
				column,
			)
		}
	}
}

func TestCommitSync_UnauthenticatedRejected(t *testing.T) {
	var githubHits int32

	fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&githubHits, 1)
		w.Write([]byte(commitPageJSON(commitSHAFixA)))
	})

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	if atomic.LoadInt32(&githubHits) != 0 {
		t.Error("GitHub was contacted for an unauthenticated commit sync")
	}
}

func TestCommitSync_MalformedSelectedRepositoryUUID(t *testing.T) {
	fixture := newCommitFixture(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/repositories/not-a-uuid/commits/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request") {
		t.Errorf("status = %d body = %q, want 400 invalid_request", recorder.Code, recorder.Body.String())
	}
}

func TestCommitSync_UnknownAndForeignUniform404(t *testing.T) {
	scenarios := []struct {
		name string
		id   uuid.UUID
	}{
		{"unknown selected repository", cmtSelectedNo},
		{"another user's selected repository", cmtSelectedFg},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var githubHits int32

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&githubHits, 1)
				w.Write([]byte(commitPageJSON(commitSHAFixA)))
			})

			fixture.addUser(cmtUserID, encryptedToken)
			fixture.addSelected(cmtSelected, cmtUserID)
			fixture.addSelected(cmtSelectedFg, cmtForeignID)

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+sc.id.String()+"/commits/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

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

			if len(fixture.store.commits) != 0 {
				t.Error("commits were persisted for an unowned repository")
			}
		})
	}
}

func TestCommitSync_TokenProblemsMapped(t *testing.T) {
	t.Run("missing stored token -> 403 github_token_missing", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)

		fixture.addUser(cmtUserID, nil)
		fixture.addSelected(cmtSelected, cmtUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "github_token_missing") {
			t.Errorf("status = %d body = %q, want 403 github_token_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupt ciphertext -> 500 internal without crypto detail", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)

		fixture.addUser(cmtUserID, strPtrList("garbage-ciphertext"))
		fixture.addSelected(cmtSelected, cmtUserID)

		req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal") {
			t.Errorf("status = %d body = %q, want 500 internal", recorder.Code, recorder.Body.String())
		}
	})
}

func TestCommitSync_GitHubFailureMappings(t *testing.T) {
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

			fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
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

			fixture.addUser(cmtUserID, encryptedToken)
			fixture.addSelected(cmtSelected, cmtUserID)

			req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
			req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != sc.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, sc.wantStatus)
			}

			if !strings.Contains(recorder.Body.String(), sc.wantError) {
				t.Errorf("body = %q, want %q", recorder.Body.String(), sc.wantError)
			}

			if len(fixture.store.commits) != 0 {
				t.Errorf("%d commits were persisted despite a failed sync", len(fixture.store.commits))
			}
		})
	}
}

func TestCommitSync_SuccessPersistsMetadataAndReturnsCleanDTO(t *testing.T) {
	var gotPath, gotRawQuery, gotAuth string

	encryptedToken, plaintext := encryptFixtureToken(t)

	fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(commitPageJSON(commitSHAFixA, commitSHAFixB)))
	})

	fixture.addUser(cmtUserID, encryptedToken)
	fixture.addSelected(cmtSelected, cmtUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if gotPath != "/repos/octocat/hello-world/commits" {
		t.Errorf("GitHub path = %q; owner/name must come from OUR authoritative selection metadata", gotPath)
	}

	query, err := url.ParseQuery(gotRawQuery)

	if err != nil {
		t.Fatalf("could not parse query: %v", err)
	}

	if query.Get("page") != "1" || query.Get("per_page") != strconv.Itoa(syncPerPage) {
		t.Errorf("query page/per_page = %q/%q", query.Get("page"), query.Get("per_page"))
	}

	if gotAuth != "Bearer "+plaintext {
		t.Errorf("GitHub Authorization used %q, want the authenticated user's decrypted token", gotAuth)
	}

	var response SyncResult

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if response.RepositoryID != cmtSelected.String() || response.Synced != 2 {
		t.Errorf("response = %+v, want {repository_id:%s synced:2}", response, cmtSelected)
	}

	raw := recorder.Body.String()

	if strings.Contains(raw, plaintext) || strings.Contains(raw, string(*encryptedToken)) {
		t.Error("response leaks token material (plain or ciphertext)")
	}

	for key, stored := range fixture.store.commits {
		if key.selectedRepositoryID != cmtSelected {
			t.Errorf("commit persisted under wrong context %v", key)
		}

		if len(stored.GithubCommitSha) != 40 ||
			stored.Message == "" || stored.AuthorName == "" || stored.HTMLURL == "" ||
			stored.GithubAuthoredAt.IsZero() || stored.GithubCommittedAt.IsZero() ||
			stored.SyncedAt.IsZero() {
			t.Errorf("stored commit = %+v, missing required metadata", stored)
		}

		if stored.GithubCommitSha != strings.ToLower(stored.GithubCommitSha) {
			t.Errorf("stored SHA %q is not lowercase", stored.GithubCommitSha)
		}
	}
}

func TestCommitSync_PaginationBoundedToMaxPages(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	const totalFakePages = 30 // beyond syncMaxPages

	var requests int32

	fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))

		if page == int(n) && n < totalFakePages {
			w.Header().Set(
				"Link",
				fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next"`, r.Host, r.URL.Path, int(n)+1),
			)
		}

		// Pad a deterministic per-page value to exactly 40 hex chars.
		pageSHA := fmt.Sprintf("%08x%04d", n, n*7919)
		pageSHA = (pageSHA + strings.Repeat("a", 40))[:40]

		w.Write([]byte(commitPageJSON(pageSHA)))
	})

	fixture.addUser(cmtUserID, encryptedToken)
	fixture.addSelected(cmtSelected, cmtUserID)

	req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

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
		t.Errorf("synced = %d, want %d (one commit per fetched page)", response.Synced, syncMaxPages)
	}
}

func TestCommitSync_RepeatedSyncDoesNotDuplicateAndRefreshesMetadata(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	syncCount := 0

	fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		syncCount++

		body := commitPageJSON(commitSHAFixA, commitSHAFixB)

		if syncCount >= 2 {
			// GitHub-side change between syncs: the author linked their
			// email to an account, so a login now appears on commit A.
			body = `[{"sha":"` + commitSHAFixA + `","author":{"login":"newly-linked"},` +
				`"html_url":"https://github.com/octocat/hello-world/commit/` + commitSHAFixA[:12] + `",` +
				`"commit":{"message":"Commit ` + commitSHAFixA[:7] + `","author":{"name":"Alice Author","email":"alice@example.com","date":"2026-01-02T03:04:05Z"},"committer":{"name":"Committer One","email":"c@example.com","date":"2026-02-03T04:05:06Z"}}}]`
		}

		w.Write([]byte(body))
	})

	fixture.addUser(cmtUserID, encryptedToken)
	fixture.addSelected(cmtSelected, cmtUserID)

	sync := func() SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

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

	if first.Synced != 2 || second.Synced != 1 {
		t.Fatalf("sync counts = %d/%d, want 2/1", first.Synced, second.Synced)
	}

	if len(fixture.store.commits) != 2 {
		t.Fatalf("stored rows = %d, want exactly 2 after repeated sync (no duplicates)", len(fixture.store.commits))
	}

	refreshed := fixture.store.commits[commitKey{cmtSelected, commitSHAFixA}]

	if refreshed.AuthorLogin != "newly-linked" {
		t.Errorf("re-sync did not refresh the linked account: %+v", refreshed)
	}
}

func TestCommitSync_CrossUserIsolationOnSameGithubRepository(t *testing.T) {
	const (
		tokenPlaintextA = "ghs_commit-isolation-user-a-token-aaaaaaa"
		tokenPlaintextB = "ghs_commit-isolation-user-b-token-bbbbbbb"
	)

	encryptedA, err := users.Encrypt(tokenPlaintextA, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt A: %v", err)
	}

	encryptedB, err := users.Encrypt(tokenPlaintextB, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt B: %v", err)
	}

	fixture := newCommitFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			switch r.Header.Get("Authorization") {
			case "Bearer " + tokenPlaintextA:
				w.Write([]byte(commitPageJSON(commitSHAFixA)))
			case "Bearer " + tokenPlaintextB:
				w.Write([]byte(commitPageJSON(commitSHAFixA, commitSHAFixB)))
			default:
				t.Error("unexpected Authorization header reaching GitHub")
				w.WriteHeader(http.StatusUnauthorized)
			}
		},
	)

	fixture.addUser(cmtUserID, &encryptedA)
	fixture.addUser(cmtForeignID, &encryptedB)
	fixture.addSelected(cmtSelected, cmtUserID)
	fixture.addSelected(cmtSelectedFg, cmtForeignID)

	run := func(selected uuid.UUID, id uuid.UUID) SyncResult {
		req := httptest.NewRequest(http.MethodPost, "/repositories/"+selected.String()+"/commits/sync", nil)
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

	resultA := run(cmtSelected, cmtUserID)
	resultB := run(cmtSelectedFg, cmtForeignID)

	if resultA.Synced != 1 || resultB.Synced != 2 {
		t.Errorf("per-user sync results = %d / %d, want 1 and 2", resultA.Synced, resultB.Synced)
	}

	countFor := func(context uuid.UUID) int {
		total := 0

		for key := range fixture.store.commits {
			if key.selectedRepositoryID == context {
				total++
			}
		}

		return total
	}

	if countFor(cmtSelected) != 1 || countFor(cmtSelectedFg) != 2 {
		t.Errorf("context isolation broken: own=%d foreign=%d, want 1 and 2",
			countFor(cmtSelected), countFor(cmtSelectedFg))
	}
}

func TestCommitSync_CancelledContextPersistsNothing(t *testing.T) {
	encryptedToken, _ := encryptFixtureToken(t)

	fixture := newCommitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(commitPageJSON(commitSHAFixA)))
	})

	fixture.addUser(cmtUserID, encryptedToken)
	fixture.addSelected(cmtSelected, cmtUserID)

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+cmtSelected.String()+"/commits/sync", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token(t, cmtUserID))

	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, request)

	if len(fixture.store.commits) != 0 {
		t.Error("commits were persisted despite a cancelled request context")
	}
}
