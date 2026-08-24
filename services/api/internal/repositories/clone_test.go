package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/discovery"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/users"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// newTestService builds a Service with an isolated workspace manager and a
// recording cloner. Used by every pre-existing fixture so constructor changes
// stay in one place.
func newTestService(
	t *testing.T,
	userService *users.Service,
	client *github.Client,
	store Store,
) *Service {
	t.Helper()

	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))

	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	return NewService(userService, client, store, testEncryptionKey(), workspaces, &fakeCloner{}, discovery.NewService(discovery.Options{}))
}

// fakeCloner records Clone calls; fn optionally simulates outcomes.
type fakeCloner struct {
	calls []fakeCloneCall
	fn    func(call fakeCloneCall) error
}

type fakeCloneCall struct {
	URL    string
	Branch string
	Token  string
	Dest   string
}

func (f *fakeCloner) Clone(
	_ context.Context,
	cloneURL string,
	branch string,
	accessToken string,
	destDir string,
) error {
	call := fakeCloneCall{
		URL:    cloneURL,
		Branch: branch,
		Token:  accessToken,
		Dest:   destDir,
	}

	f.calls = append(f.calls, call)

	if f.fn != nil {
		return f.fn(call)
	}

	return nil
}

// cloneFixture wires service + handler + clone route with a mock GitHub API.
type cloneFixture struct {
	router     *gin.Engine
	store      *fakeStore
	repo       *fakeListRepository
	service    *Service
	cloner     *fakeCloner
	jwtManager *auth.JWTManager
}

const (
	cloneTokenPlaintext = "ghs_super_secret_clone_token_42"
)

var (
	cloneUserID     = uuid.MustParse("aaaaaaaa-0f00-0000-0000-000000000001")
	cloneForeignID  = uuid.MustParse("aaaaaaaa-0f00-0000-0000-000000000002")
	cloneSelected   = uuid.MustParse("bbbbbbbb-0f00-0000-0000-000000000001")
	cloneSelectedFg = uuid.MustParse("bbbbbbbb-0f00-0000-0000-000000000002")
)

func newCloneFixture(t *testing.T, githubHandler http.HandlerFunc) *cloneFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	repo := &fakeListRepository{users: make(map[uuid.UUID]*users.User)}

	var githubMock *httptest.Server

	if githubHandler != nil {
		githubMock = httptest.NewServer(githubHandler)
	} else {
		githubMock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}))
	}

	t.Cleanup(githubMock.Close)

	store := &fakeStore{
		rows:         make(map[uuid.UUID]SelectedRepository),
		issues:       make(map[issueKey]RepositoryIssue),
		pullRequests: make(map[pullRequestKey]RepositoryPullRequest),
		commits:      make(map[commitKey]RepositoryCommit),
	}

	ciphertext, err := users.Encrypt(cloneTokenPlaintext, testEncryptionKey())

	if err != nil {
		t.Fatalf("encrypt fixture token: %v", err)
	}

	repo.users[cloneUserID] = &users.User{
		ID:                cloneUserID,
		GithubID:          583231,
		Username:          "octocat",
		GitHubAccessToken: &ciphertext,
	}
	repo.users[cloneForeignID] = &users.User{
		ID:       cloneForeignID,
		GithubID: 583232,
		Username: "hubot",
	}

	userService := users.NewService(repo)
	client := github.NewClient(nil, github.WithBaseURL(githubMock.URL))

	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))

	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	cloner := &fakeCloner{}
	service := NewService(userService, client, store, testEncryptionKey(), workspaces, cloner, discovery.NewService(discovery.Options{}))
	service.ConfigureCloneURLPolicy(workspace.DefaultAllowedHosts, true)

	jwtManager := auth.NewJWTManager([]byte(testJWTSecret))
	handler := NewHandler(service)

	router := gin.New()
	router.POST(
		"/repositories/:id/clone",
		auth.RequireAuth(jwtManager),
		handler.Clone,
	)
	router.POST(
		"/repositories/:id/discover",
		auth.RequireAuth(jwtManager),
		handler.Discover,
	)

	fixture := &cloneFixture{
		router:     router,
		store:      store,
		repo:       repo,
		service:    service,
		cloner:     cloner,
		jwtManager: jwtManager,
	}

	fixture.addSelected(cloneSelected, cloneUserID)
	fixture.addSelected(cloneSelectedFg, cloneForeignID)

	return fixture
}

func (f *cloneFixture) addSelected(id uuid.UUID, userID uuid.UUID) {
	f.store.rows[id] = SelectedRepository{
		ID:                 id,
		UserID:             userID,
		GithubRepositoryID: 1296269,
		Name:               "hello-world",
		FullName:           "octocat/hello-world",
		OwnerLogin:         "octocat",
		DefaultBranch:      "master",
	}
}

func (f *cloneFixture) call(t *testing.T, jwtToken string, selectedID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID.String()+"/clone", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	f.router.ServeHTTP(recorder, request)

	return recorder
}

func (f *cloneFixture) tokenFor(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	token, err := f.jwtManager.Issue(userID, time.Hour)

	if err != nil {
		t.Fatalf("issue test jwt: %v", err)
	}

	return token
}

// initLocalGitRepo creates a real local Git repository so unit tests exercise
// genuine go-git cloning over file:// (enabled per-fixture via
// ConfigureCloneURLPolicy).
func initLocalGitRepo(t *testing.T, dir string) string {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir source repo: %v", err)
	}

	repository, err := git.PlainInit(dir, false)

	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	readmePath := filepath.Join(dir, "README.md")

	if err := os.WriteFile(readmePath, []byte("aevor clone target"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	tree, err := repository.Worktree()

	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	if _, err := tree.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Aevor Test", Email: "test@aevor.local", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	return dir
}

// githubRepoResponse serves the authoritative GET /repositories/{id} payload.
func githubRepoResponse(t *testing.T, cloneURL string) http.HandlerFunc {
	t.Helper()

	payload, err := json.Marshal(github.Repository{
		ID:            1296269,
		Name:          "hello-world",
		FullName:      "octocat/hello-world",
		Owner:         github.RepositoryOwner{Login: "octocat"},
		Private:       false,
		DefaultBranch: "master",
		HTMLURL:       "https://github.com/octocat/hello-world",
		CloneURL:      cloneURL,
	})

	if err != nil {
		t.Fatalf("marshal repository payload: %v", err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}
}

func TestClone_HappyPathRealClone(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	// Real go-git cloner (depth 0: file transport does not do shallow).
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	result, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if err != nil {
		t.Fatalf("CloneRepository() error: %v", err)
	}

	if result.Status != "ready" || result.RepositoryID != cloneSelected.String() {
		t.Errorf("result = %+v, want ready status for %s", result, cloneSelected)
	}

	manager := fixture.service.workspaces

	if ready, err := manager.Ready(cloneSelected); err != nil || !ready {
		t.Errorf("workspace not ready after successful clone (ready=%v, err=%v)", ready, err)
	}

	configBytes, err := os.ReadFile(filepath.Join(manager.Dir(cloneSelected), ".git", "config"))

	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}

	// SECURITY: the persisted clone URL must be credential-free.
	if strings.Contains(string(configBytes), cloneTokenPlaintext) {
		t.Errorf(".git/config contains the GitHub access token")
	}

	if _, err := os.Stat(filepath.Join(manager.Dir(cloneSelected), "README.md")); err != nil {
		t.Errorf("working tree file missing after clone: %v", err)
	}

	if len(fixture.cloner.calls) != 0 {
		t.Errorf("fakeCloner unexpectedly used in real-clone test")
	}
}

func TestClone_IdempotentWhenWorkspaceReady(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("first clone: %v", err)
	}

	result, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if err != nil {
		t.Fatalf("second clone: %v", err)
	}

	if result.Status != "ready" {
		t.Errorf("second result = %+v, want ready", result)
	}

	if len(fixture.cloner.calls) != 0 {
		t.Errorf("cloner called %d times across two clones, want 0 re-clones of a verified workspace", len(fixture.cloner.calls))
	}
}

func TestClone_CorruptedWorkspaceRepairedBeforeRetry(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)
	manager := fixture.service.workspaces

	junkDir, err := manager.Reset(cloneSelected)

	if err != nil {
		t.Fatalf("seed corrupted workspace: %v", err)
	}

	junkFile := filepath.Join(junkDir, "leftover-from-crashed-run.tmp")

	if err := os.WriteFile(junkFile, []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write junk file: %v", err)
	}

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("clone over corrupted workspace: %v", err)
	}

	if _, err := os.Stat(junkFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corruption survived reset: %v", err)
	}

	if ready, err := manager.Ready(cloneSelected); err != nil || !ready {
		t.Errorf("workspace not ready after repair (ready=%v, err=%v)", ready, err)
	}
}

func TestClone_FailureDiscardsPartialWorkspace(t *testing.T) {
	var capturedDest string

	fixture := newCloneFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	fixture.service.cloner = &fakeCloner{
		fn: func(call fakeCloneCall) error {
			capturedDest = call.Dest
			return fmt.Errorf("%w: simulated transport failure", workspace.ErrCloneFailed)
		},
	}

	_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if !errors.Is(err, workspace.ErrCloneFailed) {
		t.Fatalf("error = %v, want ErrCloneFailed", err)
	}

	if _, statErr := os.Stat(capturedDest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("partial workspace survived failure at %s: %v", capturedDest, statErr)
	}
}

func TestClone_RejectedCredentialsMapToErrAuthRejected(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	fixture.service.cloner = &fakeCloner{
		fn: func(call fakeCloneCall) error {
			return fmt.Errorf("%w: bad credentials", workspace.ErrAuthRejected)
		},
	}

	_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if !errors.Is(err, workspace.ErrAuthRejected) {
		t.Fatalf("error = %v, want ErrAuthRejected", err)
	}
}

func TestClone_TimeoutMapsToErrTimeout(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	fixture.service.cloneTimeout = time.Nanosecond
	fixture.service.cloner = &fakeCloner{
		fn: func(call fakeCloneCall) error {
			time.Sleep(time.Millisecond)

			return fmt.Errorf("fetch interrupted: %w", context.DeadlineExceeded)
		},
	}

	_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if !errors.Is(err, workspace.ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

func TestClone_InvalidAuthoritativeCloneURLIsRejected(t *testing.T) {
	for name, cloneURL := range map[string]string{
		"http scheme":        "http://github.com/octocat/hello-world.git",
		"disallowed host":    "https://evil.example.com/octocat/hello-world.git",
		"ssh scheme":         "git@github.com:octocat/hello-world.git",
		"userinfo injection": "https://token@github.com/octocat/hello-world.git",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCloneFixture(t, githubRepoResponse(t, cloneURL))
			// file:// NOT allowed for these cases.
			fixture.service.ConfigureCloneURLPolicy(workspace.DefaultAllowedHosts, false)

			_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

			if !errors.Is(err, workspace.ErrInvalidCloneURL) {
				t.Fatalf("error = %v, want ErrInvalidCloneURL", err)
			}

			if len(fixture.cloner.calls) != 0 {
				t.Errorf("cloner invoked despite policy violation")
			}
		})
	}
}

func TestClone_ForeignAndUnknownIDsAreNotFound(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelectedFg); !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("foreign id error = %v, want ErrSelectedNotFound", err)
	}

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, uuid.New()); !errors.Is(err, ErrSelectedNotFound) {
		t.Errorf("unknown id error = %v, want ErrSelectedNotFound", err)
	}
}

func TestClone_MissingGitHubToken(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	delete(fixture.repo.users, cloneUserID)
	fixture.repo.users[cloneUserID] = &users.User{
		ID:       cloneUserID,
		GithubID: 583231,
		Username: "octocat",
	}

	_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if !errors.Is(err, users.ErrGitHubTokenMissing) {
		t.Fatalf("error = %v, want ErrGitHubTokenMissing", err)
	}
}

func TestClone_GitHubRepositoryVanished(t *testing.T) {
	fixture := newCloneFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})

	_, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected)

	if !errors.Is(err, github.ErrRepositoryNotFound) {
		t.Fatalf("error = %v, want ErrRepositoryNotFound", err)
	}
}

func TestClone_HandlerContract(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := fixture.call(t, "", cloneSelected)

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("happy path returns identifiers only", func(t *testing.T) {
		fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)
		recorder := fixture.call(t, fixture.tokenFor(t, cloneUserID), cloneSelected)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}

		body := recorder.Body.String()

		if !strings.Contains(body, `"repository_id":"`+cloneSelected.String()+`"`) ||
			!strings.Contains(body, `"status":"ready"`) {
			t.Errorf("body = %s, want repository_id and ready status only", body)
		}

		if strings.Contains(body, cloneTokenPlaintext) ||
			strings.Contains(body, source) ||
			strings.Contains(body, "file://") {
			t.Errorf("body leaks sensitive material: %s", body)
		}
	})

	t.Run("foreign repository is indistinguishable 404", func(t *testing.T) {
		recorder := fixture.call(t, fixture.tokenFor(t, cloneUserID), cloneSelectedFg)

		if recorder.Code != http.StatusNotFound ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
			t.Errorf("status=%d body=%s, want opaque repository_not_found", recorder.Code, recorder.Body.String())
		}
	})
}
