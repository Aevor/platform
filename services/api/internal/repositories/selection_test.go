package repositories

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm/schema"

	"github.com/Aevor/platform/services/api/internal/users"
)

// fakeStore is an in-memory Store double that records calls so tests can
// assert what the service attempted to persist.
type fakeStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]SelectedRepository
	// issues emulates the (selected_repository_id, github_issue_id) unique
	// constraint of repository_issues: key is that composite pair.
	issues    map[issueKey]RepositoryIssue
	upserts   int
	deletes   int
	upsertErr error
	deleteErr error
}

type issueKey struct {
	selectedRepositoryID uuid.UUID
	githubIssueID        int64
}

func (f *fakeStore) UpsertSelected(repository *SelectedRepository) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.upserts++

	if f.upsertErr != nil {
		return f.upsertErr
	}

	// Emulate the real unique index on (user_id, github_repository_id):
	// re-selecting a known repository updates the existing row in place.
	for id, row := range f.rows {
		if row.UserID == repository.UserID && row.GithubRepositoryID == repository.GithubRepositoryID {
			repository.ID = id
			repository.CreatedAt = row.CreatedAt
			f.rows[id] = *repository

			return nil
		}
	}

	if repository.ID == uuid.Nil {
		repository.ID = uuid.New()
	}

	if repository.CreatedAt.IsZero() {
		repository.CreatedAt = time.Now()
	}

	f.rows[repository.ID] = *repository

	return nil
}

func (f *fakeStore) ListByUserID(userID uuid.UUID) ([]SelectedRepository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	result := make([]SelectedRepository, 0)

	for _, row := range f.rows {
		if row.UserID == userID {
			result = append(result, row)
		}
	}

	return result, nil
}

func (f *fakeStore) DeleteByUserAndID(userID uuid.UUID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deletes++

	if f.deleteErr != nil {
		return f.deleteErr
	}

	row, ok := f.rows[id]

	if !ok || row.UserID != userID {
		return ErrSelectedNotFound
	}

	delete(f.rows, id)

	return nil
}

func (f *fakeStore) FindByUserAndID(userID uuid.UUID, id uuid.UUID) (*SelectedRepository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	row, ok := f.rows[id]

	if !ok || row.UserID != userID {
		return nil, ErrSelectedNotFound
	}

	return &row, nil
}

func (f *fakeStore) UpsertIssues(selectedRepositoryID uuid.UUID, issues []RepositoryIssue) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range issues {
		issues[i].SelectedRepositoryID = selectedRepositoryID
		issues[i].ID = uuid.Nil

		key := issueKey{selectedRepositoryID, issues[i].GithubIssueID}

		if existing, ok := f.issues[key]; ok {
			issues[i].ID = existing.ID
			issues[i].CreatedAt = existing.CreatedAt
		} else {
			issues[i].ID = uuid.New()
			issues[i].CreatedAt = time.Now()
		}

		f.issues[key] = issues[i]
	}

	return nil
}

func githubRepoJSON(id int64) string {
	return `{
		"id": ` + jsonInt64(id) + `,
		"name": "hello-world",
		"full_name": "octocat/hello-world",
		"private": true,
		"description": "ignored by selection",
		"default_branch": "main",
		"owner": {"login": "octocat"},
		"html_url": "https://github.com/octocat/hello-world",
		"clone_url": "https://github.com/octocat/hello-world.git",
		"url": "https://api.github.com/repos/octocat/hello-world"
	}`
}

func jsonInt64(v int64) string {
	raw, _ := json.Marshal(v)

	return string(raw)
}

// encryptFixtureToken returns (pointer to encrypted ciphertext, plaintext) for
// a deterministic test token, encrypted with the fixture encryption key.
func encryptFixtureToken(t *testing.T) (*string, string) {
	t.Helper()

	const plaintext = testPlaintextToken

	encrypted, err := users.Encrypt(plaintext, testEncryptionKey())

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	return &encrypted, plaintext
}

func TestUpsertSelected_ColumnsMatchModelSchema(t *testing.T) {
	userSchema, err := schema.Parse(&SelectedRepository{}, &sync.Map{}, schema.NamingStrategy{})

	if err != nil {
		t.Fatalf("could not parse the SelectedRepository model schema: %v", err)
	}

	columns := make(map[string]bool)

	for _, field := range userSchema.Fields {
		if field.DBName != "" {
			columns[field.DBName] = true
		}
	}

	for _, column := range upsertConflictColumns {
		if !columns[column.Name] {
			t.Errorf(
				"upsert conflict column %q is not mapped by the SelectedRepository model; ON CONFLICT would reference a nonexistent column",
				column.Name,
			)
		}
	}

	for _, column := range upsertAssignmentColumns {
		if !columns[column] {
			t.Errorf(
				"upsert assignment column %q is not mapped by the SelectedRepository model; DO UPDATE SET would fail at runtime",
				column,
			)
		}
	}
}

func TestSelect_UnauthenticatedRejected(t *testing.T) {
	var githubHit bool

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		githubHit = true
		w.Write([]byte(githubRepoJSON(1296269)))
	}, fixtureUser(uuid.New(), nil))

	req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(`{"github_repository_id":1296269}`))
	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}

	if githubHit {
		t.Error("GitHub API was contacted for an unauthenticated selection")
	}
}

func TestSelect_InvalidRequestRejected(t *testing.T) {
	scenarios := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"not json", `{nope`},
		{"missing id", `{}`},
		{"zero id", `{"github_repository_id":0}`},
		{"negative id", `{"github_repository_id":-5}`},
		{"string id", `{"github_repository_id":"abc"}`},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			userID := uuid.New()

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(githubRepoJSON(1296269)))
			}, fixtureUser(userID, encryptedToken))

			req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(sc.body))
			req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", recorder.Code)
			}

			if !strings.Contains(recorder.Body.String(), "invalid_request") {
				t.Errorf("body = %q, want invalid_request", recorder.Body.String())
			}

			if fixture.store.upserts != 0 {
				t.Errorf("store received %d upserts for an invalid request", fixture.store.upserts)
			}
		})
	}
}

func TestSelect_SuccessPersistsAuthoritativeMetadata(t *testing.T) {
	userID := uuid.New()

	var gotPath, gotAuth string

	encryptedToken, plaintext := encryptFixtureToken(t)

	fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(githubRepoJSON(1296269)))
	}, fixtureUser(userID, encryptedToken))

	body := `{"github_repository_id":1296269}`

	req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}

	if gotPath != "/repositories/1296269" {
		t.Errorf("GitHub path = %q, want /repositories/1296269 (authoritative lookup)", gotPath)
	}

	if gotAuth != "Bearer "+plaintext {
		t.Errorf("GitHub Authorization used %q, want the authenticated user's decrypted token", gotAuth)
	}

	var response SelectedRepositoryResponse

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if response.ID == "" || response.GithubRepositoryID != 1296269 ||
		response.FullName != "octocat/hello-world" || response.OwnerLogin != "octocat" ||
		response.DefaultBranch != "main" || !response.Private {
		t.Errorf("response = %+v, want the authoritative GitHub metadata", response)
	}

	if len(fixture.store.rows) != 1 {
		t.Fatalf("store rows = %d, want exactly 1", len(fixture.store.rows))
	}

	for _, stored := range fixture.store.rows {
		if stored.UserID != userID || stored.GithubRepositoryID != 1296269 || stored.Name != "hello-world" {
			t.Errorf("stored record = %+v, want ownership bound to the authenticated user", stored)
		}
	}

	if strings.Contains(recorder.Body.String(), plaintext) {
		t.Error("response contains the GitHub access token")
	}
}

func TestSelect_InaccessibleRepositoryRejected(t *testing.T) {
	scenarios := []struct {
		name       string
		githubCode int
	}{
		{"repository does not exist", http.StatusNotFound},
		{"repository not visible to account", http.StatusNotFound},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			userID := uuid.New()

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"Not Found"}`))
			}, fixtureUser(userID, encryptedToken))

			req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(`{"github_repository_id":999}`))
			req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}

			if !strings.Contains(recorder.Body.String(), "repository_not_found") {
				t.Errorf("body = %q, want repository_not_found", recorder.Body.String())
			}

			if len(fixture.store.rows) != 0 {
				t.Error("an inaccessible repository was persisted")
			}
		})
	}
}

func TestSelect_GitHubFailureMappings(t *testing.T) {
	scenarios := []struct {
		name         string
		githubStatus int
		wantStatus   int
		wantError    string
	}{
		{"token rejected", http.StatusUnauthorized, http.StatusUnauthorized, "github_token_invalid"},
		{"rate limited", http.StatusTooManyRequests, http.StatusTooManyRequests, "github_rate_limited"},
		{"github down", http.StatusInternalServerError, http.StatusInternalServerError, "github_unavailable"},
		{"malformed payload", http.StatusOK, http.StatusInternalServerError, "github_unavailable"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			userID := uuid.New()

			encryptedToken, _ := encryptFixtureToken(t)

			fixture := newListFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.githubStatus)

				if sc.githubStatus == http.StatusOK {
					w.Write([]byte(`{not-json`))
				} else if sc.githubStatus != http.StatusNotFound {
					w.Write([]byte(`{"message":"nope"}`))
				}
			}, fixtureUser(userID, encryptedToken))

			req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(`{"github_repository_id":7}`))
			req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))

			recorder := httptest.NewRecorder()

			fixture.router.ServeHTTP(recorder, req)

			if recorder.Code != sc.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, sc.wantStatus)
			}

			if !strings.Contains(recorder.Body.String(), sc.wantError) {
				t.Errorf("body = %q, want %q", recorder.Body.String(), sc.wantError)
			}

			if len(fixture.store.rows) != 0 {
				t.Error("a failed selection was persisted")
			}
		})
	}
}

func TestSelect_TokenProblemsMapped(t *testing.T) {
	t.Run("missing stored token -> 403", func(t *testing.T) {
		userID := uuid.New()

		fixture := newListFixture(t, nil, fixtureUser(userID, nil))

		req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(`{"github_repository_id":7}`))
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "github_token_missing") {
			t.Errorf("status = %d body = %q, want 403 github_token_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("undecryptable stored token -> 500 internal", func(t *testing.T) {
		userID := uuid.New()

		fixture := newListFixture(t, nil, fixtureUser(userID, strPtrList("garbage-ciphertext")))

		req := httptest.NewRequest(http.MethodPost, "/repositories", strings.NewReader(`{"github_repository_id":7}`))
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userID))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal") {
			t.Errorf("status = %d body = %q, want 500 internal", recorder.Code, recorder.Body.String())
		}
	})
}

func TestListSelected_UserIsolation(t *testing.T) {
	userA := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	userB := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	encryptedToken, _ := encryptFixtureToken(t)

	fixture := newListFixture(t, nil,
		fixtureUser(userA, encryptedToken),
		fixtureUser(userB, encryptedToken),
	)

	fixture.store.rows[uuid.MustParse("66666666-6666-6666-6666-666666666661")] = SelectedRepository{
		ID:                 uuid.MustParse("66666666-6666-6666-6666-666666666661"),
		UserID:             userA,
		GithubRepositoryID: 1001,
		Name:               "a-repo-one",
		FullName:           "octocat/a-repo-one",
	}
	fixture.store.rows[uuid.MustParse("66666666-6666-6666-6666-666666666662")] = SelectedRepository{
		ID:                 uuid.MustParse("66666666-6666-6666-6666-666666666662"),
		UserID:             userA,
		GithubRepositoryID: 1002,
		Name:               "a-repo-two",
		FullName:           "octocat/a-repo-two",
	}
	fixture.store.rows[uuid.MustParse("66666666-6666-6666-6666-666666666663")] = SelectedRepository{
		ID:                 uuid.MustParse("66666666-6666-6666-6666-666666666663"),
		UserID:             userB,
		GithubRepositoryID: 1001,
		Name:               "b-repo-same-github-id",
		FullName:           "octocat/a-repo-one",
	}

	req := httptest.NewRequest(http.MethodGet, "/repositories/selected", nil)
	req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userA))

	recorder := httptest.NewRecorder()

	fixture.router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var response struct {
		Repositories []SelectedRepositoryResponse `json:"repositories"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if len(response.Repositories) != 2 {
		t.Fatalf("user A saw %d repositories, want exactly their own 2", len(response.Repositories))
	}

	for _, repository := range response.Repositories {
		if repository.GithubRepositoryID == 1001 && repository.ID == "66666666-6666-6666-6666-666666666663" {
			t.Error("user B's record leaked into user A's response")
		}
	}
}

func TestDelete_RemovesOwnRecordOnly(t *testing.T) {
	userA := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	userB := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	encryptedToken, _ := encryptFixtureToken(t)

	fixture := newListFixture(t, nil,
		fixtureUser(userA, encryptedToken),
		fixtureUser(userB, encryptedToken),
	)

	ownedID := uuid.MustParse("77777777-7777-7777-7777-777777777701")

	fixture.store.rows[ownedID] = SelectedRepository{ID: ownedID, UserID: userA, GithubRepositoryID: 2001}

	t.Run("owner deletes -> 204", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/repositories/"+ownedID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userA))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", recorder.Code)
		}

		if _, stillThere := fixture.store.rows[ownedID]; stillThere {
			t.Error("record was not deleted")
		}
	})

	t.Run("other user's record -> 404 and survives", func(t *testing.T) {
		foreignID := uuid.MustParse("77777777-7777-7777-7777-777777777002")

		fixture.store.rows[foreignID] = SelectedRepository{ID: foreignID, UserID: userB, GithubRepositoryID: 2002}

		req := httptest.NewRequest(http.MethodDelete, "/repositories/"+foreignID.String(), nil)
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userA))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for a foreign record", recorder.Code)
		}

		if _, stillThere := fixture.store.rows[foreignID]; !stillThere {
			t.Error("a foreign record was deleted")
		}
	})

	t.Run("unknown id -> 404", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodDelete,
			"/repositories/88888888-8888-8888-8888-888888888888",
			nil,
		)
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userA))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for an unknown id", recorder.Code)
		}
	})

	t.Run("invalid uuid -> 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/repositories/not-a-uuid", nil)
		req.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, userA))

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for a malformed id", recorder.Code)
		}
	})

	t.Run("unauthenticated -> 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/repositories/"+ownedID.String(), nil)

		recorder := httptest.NewRecorder()

		fixture.router.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", recorder.Code)
		}
	})
}

func TestService_UpsertRefreshesMetadataOnDuplicateSelection(t *testing.T) {
	store := &fakeStore{rows: make(map[uuid.UUID]SelectedRepository), issues: make(map[issueKey]RepositoryIssue)}

	first := &SelectedRepository{UserID: uuid.New(), GithubRepositoryID: 42, Name: "old-name"}

	if err := store.UpsertSelected(first); err != nil {
		t.Fatalf("first upsert error: %v", err)
	}

	refreshed := &SelectedRepository{
		UserID:             first.UserID,
		GithubRepositoryID: 42,
		Name:               "renamed",
		FullName:           "octocat/renamed",
	}

	if err := store.UpsertSelected(refreshed); err != nil {
		t.Fatalf("second upsert error: %v", err)
	}

	stored := store.rows[first.ID]

	if stored.Name != "renamed" {
		t.Errorf("duplicate selection kept stale metadata %q; want refreshed metadata", stored.Name)
	}

	if len(store.rows) != 1 {
		t.Errorf("row count = %d, want 1 (same user + same repo must not duplicate)", len(store.rows))
	}
}

func TestStore_DeleteUnknownReturnsSentinel(t *testing.T) {
	store := &fakeStore{rows: make(map[uuid.UUID]SelectedRepository), issues: make(map[issueKey]RepositoryIssue)}

	err := store.DeleteByUserAndID(uuid.New(), uuid.New())

	if !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("error = %v, want ErrSelectedNotFound", err)
	}
}
