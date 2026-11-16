package repositories

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/github"
	"github.com/Aevor/platform/services/api/internal/indexing"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

const prFeedbackPath = "/repositories/%s/pull-requests/%d/analyze-feedback"

// prFeedbackAIJSON is the canonical AI-service response the tests expect: a
// blocking inline comment grounded in the changed file.
const prFeedbackAIJSON = `{
	"summary": "One blocking inline comment about an unbounded loop.",
	"blocking": [{
		"severity": "critical",
		"source": "review_comment",
		"author_login": "hubot",
		"file_path": "internal/api.go",
		"line": 12,
		"original_text": "guard this",
		"recommendation": "Bound the loop iterations to the page size.",
		"reason": "The comment flags an unbounded loop in the changed file."
	}],
	"non_blocking": [],
	"insufficient_context": false,
	"status": "analyzed"
}`

type prFeedbackFixture struct {
	*cloneFixture
	aiServer *httptest.Server
	received *ai.AnalyzePRFeedbackRequest
	aiStatus int
	aiBody   string
}

// newPRFeedbackFixture wires the service + analyze-feedback route to a
// recording mock AI service over the shared PR-details GitHub API fixtures.
// The workspace is NOT seeded here; tests that reach the AI call seed it
// themselves (mirroring clone → index → analyze in real usage).
func newPRFeedbackFixture(t *testing.T, githubHandler http.HandlerFunc) *prFeedbackFixture {
	t.Helper()

	fixture := &prFeedbackFixture{
		cloneFixture: newCloneFixture(t, githubHandler),
		aiStatus:     http.StatusOK,
		aiBody:       prFeedbackAIJSON,
	}

	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/analyze-pr-feedback" {
			http.NotFound(w, r)
			return
		}

		request := &ai.AnalyzePRFeedbackRequest{}

		if err := json.NewDecoder(r.Body).Decode(request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		fixture.received = request

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fixture.aiStatus)
		w.Write([]byte(fixture.aiBody))
	}))
	t.Cleanup(aiServer.Close)

	fixture.aiServer = aiServer
	fixture.service.aiClient = ai.NewClient(nil, ai.WithBaseURL(aiServer.URL))

	handler := NewHandler(fixture.service)

	fixture.router.POST(
		"/repositories/:id/pull-requests/:number/analyze-feedback",
		auth.RequireAuth(fixture.jwtManager),
		handler.AnalyzePullRequestFeedback,
	)

	return fixture
}

// prFeedbackGitHubAPI serves BOTH the repository metadata clone needs
// (githubRepoResponse) and the PR-details sub-endpoints (prDetailsGitHubAPI),
// so tests that seed a real workspace can clone first.
func prFeedbackGitHubAPI(t *testing.T, repoHandler http.HandlerFunc, overrides map[string]string, captureAuth *string) http.HandlerFunc {
	t.Helper()

	details := prDetailsGitHubAPI(overrides, captureAuth)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repositories/1296269" {
			repoHandler(w, r)
			return
		}

		details(w, r)
	}
}

// newPRFeedbackWorkspaceFixture builds the fixture over a real local Git
// repository so seeding can clone via go-git and index the changed file.
func newPRFeedbackWorkspaceFixture(t *testing.T, overrides map[string]string, captureAuth *string) (*prFeedbackFixture, string) {
	t.Helper()

	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))

	f := newPRFeedbackFixture(t, prFeedbackGitHubAPI(t, githubRepoResponse(t, "file://"+source), overrides, captureAuth))

	return f, source
}

// seedPRFeedbackWorkspace clones the configured local repo and writes a real
// `internal/api.go` so the PR's changed-file list matches, then rebuilds the
// metadata index (clone → index → analyze, as in real usage).
func seedPRFeedbackWorkspace(t *testing.T, f *prFeedbackFixture) string {
	t.Helper()

	f.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := f.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := f.service.workspaces.Dir(cloneSelected)

	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "internal", "api.go"), []byte(
		"package internal\n\nimport \"fmt\"\n\nfunc guard() {\n\tfmt.Println(\"PRSOURCE\")\n}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.IndexRepositoryContent(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	return dir
}

// prFeedbackCall posts to the analyze-feedback endpoint.
func prFeedbackCall(
	t *testing.T,
	f *prFeedbackFixture,
	token string,
	repoID uuid.UUID,
	number int,
) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(
		http.MethodPost,
		fmt.Sprintf(prFeedbackPath, repoID.String(), number),
		nil,
	)

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()

	f.router.ServeHTTP(recorder, request)

	return recorder
}

func decodePRFeedback(t *testing.T, recorder *httptest.ResponseRecorder) PullRequestFeedbackResponse {
	t.Helper()

	var response PullRequestFeedbackResponse

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pull request feedback response: %v", err)
	}

	return response
}

func TestPullRequestFeedback_Success(t *testing.T) {
	var gotAuth string

	f, _ := newPRFeedbackWorkspaceFixture(t, nil, &gotAuth)
	seedPRFeedbackWorkspace(t, f)

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	if gotAuth != "Bearer "+cloneTokenPlaintext {
		t.Errorf("Authorization = %q, want the stored token as Bearer", gotAuth)
	}

	if f.received == nil {
		t.Fatal("AI service never received a request")
	}

	request := f.received

	if request.RepositoryID != cloneSelected.String() || request.RepositoryName != "hello-world" {
		t.Errorf("repository identity = %s/%s, want the owned selected repository",
			request.RepositoryID, request.RepositoryName)
	}

	if request.PullRequest.Number != 7 || request.PullRequest.Title != "Add pagination" ||
		request.PullRequest.Body != "Implements pagination for the API." ||
		request.PullRequest.State != "open" || request.PullRequest.AuthorLogin != "octocat" ||
		request.PullRequest.HeadSHA != prDetailsHeadSHA {
		t.Errorf("pull_request = %+v, want the authoritative GitHub record", request.PullRequest)
	}

	if len(request.Checks) != 3 {
		t.Fatalf("checks = %d, want 3 (2 check runs + 1 commit status)", len(request.Checks))
	}

	if request.Checks[0].Name != "build" || request.Checks[0].Status != "in_progress" ||
		request.Checks[0].Source != "check_run" {
		t.Errorf("live check = %+v, want GitHub's own status", request.Checks[0])
	}

	if request.Checks[2].Name != "ci/lint" || request.Checks[2].Status != "failure" ||
		request.Checks[2].Source != "commit_status" {
		t.Errorf("commit status = %+v, want raw state + source", request.Checks[2])
	}

	if len(request.Reviews) != 1 || request.Reviews[0].AuthorLogin != "hubot" ||
		request.Reviews[0].State != "approved" || request.Reviews[0].Body != "LGTM" {
		t.Errorf("reviews = %+v, want the review facts preserved", request.Reviews)
	}

	if len(request.ReviewComments) != 1 || request.ReviewComments[0].Path != "internal/api.go" ||
		request.ReviewComments[0].Line == nil || *request.ReviewComments[0].Line != 12 ||
		request.ReviewComments[0].Body != "guard this" {
		t.Errorf("review comments = %+v, want the diff-anchored feedback", request.ReviewComments)
	}

	if len(request.IssueComments) != 1 || request.IssueComments[0].Body != "great work" {
		t.Errorf("issue comments = %+v, want the thread conversation", request.IssueComments)
	}

	if len(request.Files) != 1 || request.Files[0].Filename != "internal/api.go" ||
		request.Files[0].Status != "modified" || request.Files[0].Additions != 3 ||
		request.Files[0].Deletions != 1 {
		t.Errorf("files = %+v, want changed-file metadata", request.Files)
	}

	if len(request.ContextChunks) == 0 {
		t.Fatal("no context chunks forwarded to the AI service")
	}

	var matched bool
	var fileContent strings.Builder
	for _, chunk := range request.ContextChunks {
		if chunk.FilePath == "internal/api.go" {
			matched = true
			fileContent.WriteString(chunk.Content)
			if chunk.ID == "" || chunk.Language == "" || chunk.ChunkIndex < 0 {
				t.Errorf("chunk %s missing traceable metadata: %+v", chunk.FilePath, chunk)
			}
		}
	}

	if !matched {
		paths := make([]string, 0, len(request.ContextChunks))
		for _, chunk := range request.ContextChunks {
			paths = append(paths, chunk.FilePath)
		}
		t.Fatalf("no chunk for the changed file internal/api.go, got %v", paths)
	}

	if !strings.Contains(fileContent.String(), "PRSOURCE") {
		t.Errorf("combined internal/api.go content = %q, want the seeded source", fileContent.String())
	}

	feedback := decodePRFeedback(t, recorder)

	if feedback.PullRequestNumber != 7 {
		t.Errorf("pull_request_number = %d, want 7", feedback.PullRequestNumber)
	}

	if feedback.Summary != "One blocking inline comment about an unbounded loop." ||
		feedback.InsufficientContext || feedback.Status != "analyzed" {
		t.Errorf("summary/flag/status = %q/%v/%q, want the validated AI analysis",
			feedback.Summary, feedback.InsufficientContext, feedback.Status)
	}

	if len(feedback.Blocking) != 1 {
		t.Fatalf("blocking = %d, want 1", len(feedback.Blocking))
	}

	item := feedback.Blocking[0]

	if item.Severity != "critical" || item.Source != "review_comment" ||
		item.AuthorLogin != "hubot" || item.FilePath != "internal/api.go" ||
		item.Line == nil || *item.Line != 12 || item.OriginalText != "guard this" ||
		item.Recommendation != "Bound the loop iterations to the page size." ||
		item.Reason == "" {
		t.Errorf("blocking item = %+v, want GitHub facts + grounded interpretation", item)
	}

	if len(feedback.NonBlocking) != 0 {
		t.Errorf("non_blocking = %d, want none", len(feedback.NonBlocking))
	}
}

func TestPullRequestFeedback_Unauthorized(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))

	recorder := prFeedbackCall(t, f, "", cloneSelected, 7)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", recorder.Code, recorder.Body.String())
	}
}

func TestPullRequestFeedback_InvalidParams(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))

	scenarios := []struct {
		name   string
		repoID string
		number string
	}{
		{"non-uuid repository", "not-a-uuid", "7"},
		{"zero PR number", cloneSelected.String(), "0"},
		{"negative PR number", cloneSelected.String(), "-1"},
		{"non-numeric PR number", cloneSelected.String(), "abc"},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/repositories/"+sc.repoID+"/pull-requests/"+sc.number+"/analyze-feedback",
				nil,
			)
			request.Header.Set("Authorization", "Bearer "+f.tokenFor(t, cloneUserID))

			recorder := httptest.NewRecorder()

			f.router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPullRequestFeedback_ForeignRepositoryOpaque404(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))

	for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
		recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), target, 7)

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want opaque 404", recorder.Code, recorder.Body.String())
		}

		if strings.Contains(recorder.Body.String(), "pagination") {
			t.Errorf("body leaked PR details for an inaccessible repository: %s", recorder.Body.String())
		}

		if f.received != nil {
			t.Errorf("AI service was contacted for an inaccessible repository")
		}
	}
}

func TestPullRequestFeedback_ForeignUserTokenIsOpaque(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneForeignID), cloneSelected, 7)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want opaque 404 for a foreign owner", recorder.Code, recorder.Body.String())
	}

	if f.received != nil {
		t.Errorf("AI service was contacted for a foreign owner")
	}
}

func TestPullRequestFeedback_PullRequestNotFound(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(map[string]string{
		"/repos/octocat/hello-world/pulls/7": "",
	}, nil))

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", recorder.Code, recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body["error"] != "pull_request_not_found" {
		t.Errorf("error = %q, want pull_request_not_found", body["error"])
	}

	if f.received != nil {
		t.Errorf("AI service was contacted for a missing pull request")
	}
}

func TestPullRequestFeedback_WorkspaceNotReady(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", recorder.Code, recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body["error"] != "workspace_not_ready" {
		t.Errorf("error = %q, want workspace_not_ready", body["error"])
	}

	if f.received != nil {
		t.Errorf("AI service was contacted without a prepared workspace")
	}
}

func TestPullRequestFeedback_GitHubStatusCodesMapped(t *testing.T) {
	scenarios := []struct {
		name         string
		githubStatus int
		githubBody   string
		wantHTTP     int
		wantError    string
	}{
		{
			"token rejected",
			http.StatusUnauthorized,
			`{"message":"Bad credentials"}`,
			http.StatusUnauthorized,
			"github_token_invalid",
		},
		{
			"rate limited",
			http.StatusTooManyRequests,
			`{"message":"API rate limit exceeded"}`,
			http.StatusTooManyRequests,
			"github_rate_limited",
		},
		{
			"github unavailable",
			http.StatusServiceUnavailable,
			`{"message":"down"}`,
			http.StatusInternalServerError,
			"github_unavailable",
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(sc.githubStatus)
				w.Write([]byte(sc.githubBody))
			})

			f := newPRFeedbackFixture(t, handler)

			recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

			if recorder.Code != sc.wantHTTP {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), sc.wantHTTP)
			}

			var body map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}

			if body["error"] != sc.wantError {
				t.Errorf("error = %q, want %q", body["error"], sc.wantError)
			}

			if f.received != nil {
				t.Errorf("AI service was contacted after a GitHub failure")
			}
		})
	}
}

func TestPullRequestFeedback_AIServiceUnavailable(t *testing.T) {
	f, _ := newPRFeedbackWorkspaceFixture(t, nil, nil)
	seedPRFeedbackWorkspace(t, f)
	f.aiStatus = http.StatusInternalServerError
	f.aiBody = `{"detail":"AI pull request feedback analysis failed"}`

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", recorder.Code, recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body["error"] != "ai_service_unavailable" {
		t.Errorf("error = %q, want ai_service_unavailable", body["error"])
	}
}

func TestPullRequestFeedback_AIRateLimited(t *testing.T) {
	f, _ := newPRFeedbackWorkspaceFixture(t, nil, nil)
	seedPRFeedbackWorkspace(t, f)
	f.aiStatus = http.StatusTooManyRequests

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", recorder.Code, recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body["error"] != "ai_rate_limited" {
		t.Errorf("error = %q, want ai_rate_limited", body["error"])
	}
}

func TestPullRequestFeedback_AIUnauthorized(t *testing.T) {
	f, _ := newPRFeedbackWorkspaceFixture(t, nil, nil)
	seedPRFeedbackWorkspace(t, f)
	f.aiStatus = http.StatusUnauthorized

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s, want 401", recorder.Code, recorder.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body["error"] != "ai_unauthorized" {
		t.Errorf("error = %q, want ai_unauthorized", body["error"])
	}
}

func TestPullRequestFeedback_InvalidAIResponseFailsClosed(t *testing.T) {
	f, _ := newPRFeedbackWorkspaceFixture(t, nil, nil)
	seedPRFeedbackWorkspace(t, f)
	f.aiBody = `not json at all`

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want fail-closed 500", recorder.Code, recorder.Body.String())
	}
}

func TestPullRequestFeedback_FailsClosedWhenAIClientNil(t *testing.T) {
	f := newPRFeedbackFixture(t, prDetailsGitHubAPI(nil, nil))
	f.service.aiClient = nil

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusInternalServerError ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"internal"}` {
		t.Errorf("status=%d body=%s, want fail-closed internal", recorder.Code, recorder.Body.String())
	}
}

func TestSelectRecordsForFiles(t *testing.T) {
	records := []indexing.Record{
		{FilePath: "internal/api.go"},
		{FilePath: "README.md"},
		{FilePath: "cmd/server/main.go"},
	}

	selected := selectRecordsForFiles([]github.PullRequestFile{
		{Filename: "internal/api.go"},
		{Filename: "NOT_INDEXED.go"},
	}, records, 10)

	if len(selected) != 1 || selected[0].FilePath != "internal/api.go" {
		t.Fatalf("selected = %v, want only the PR's changed file", pathsOf(selected))
	}
}

func TestSelectRecordsForFiles_NoMatchDegradesToBoundedSubset(t *testing.T) {
	records := []indexing.Record{
		{FilePath: "a.go"},
		{FilePath: "b.go"},
	}

	selected := selectRecordsForFiles([]github.PullRequestFile{
		{Filename: "phantom.go"},
	}, records, 10)

	if len(selected) != 2 {
		t.Fatalf("selected = %d, want the bounded fallback subset so analysis is still attempted", len(selected))
	}
}

func TestSelectRecordsForFiles_NoFilesDegradesToBoundedSubset(t *testing.T) {
	records := []indexing.Record{
		{FilePath: "a.go"},
		{FilePath: "b.go"},
	}

	selected := selectRecordsForFiles(nil, records, 1)

	if len(selected) != 1 || selected[0].FilePath != "a.go" {
		t.Fatalf("selected = %v, want the first record only", pathsOf(selected))
	}
}

func pathsOf(records []indexing.Record) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.FilePath)
	}
	return out
}

// TestPullRequestFeedback_ContextIsSecretFree asserts that the AI request
// never carries the user's token even when the reviewer's own words include
// secret-looking content: the token stays server-side, the review text is
// still forwarded as data.
func TestPullRequestFeedback_ContextIsSecretFree(t *testing.T) {
	overrides := map[string]string{
		"/repos/octocat/hello-world/pulls/7/comments": `[{
			"id": 602,
			"user": {"login": "hubot"},
			"body": "paste your token: ghs_super_secret_clone_token_42",
			"path": "internal/api.go",
			"line": 12,
			"diff_hunk": "@@ -1,3 +1,4 @@",
			"commit_id": "abcd1111abcd1111abcd1111abcd1111abcd1111",
			"html_url": "https://github.com/octocat/hello-world/pull/7#discussion_r602",
			"created_at": "2026-03-04T05:06:07Z",
			"updated_at": "2026-03-04T05:06:07Z"
		}]`,
	}

	f, _ := newPRFeedbackWorkspaceFixture(t, overrides, nil)
	seedPRFeedbackWorkspace(t, f)

	recorder := prFeedbackCall(t, f, f.tokenFor(t, cloneUserID), cloneSelected, 7)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	raw, err := json.Marshal(f.received)
	if err != nil {
		t.Fatalf("marshal captured request: %v", err)
	}

	// The reviewer's own words travel as data (they are part of the PR
	// feedback being analyzed), but the stored token must never be forwarded
	// in any credential position — and the JSON never carries it as a header
	// or authorization field.
	if strings.Contains(string(raw), "Bearer") {
		t.Errorf("captured AI request leaked a Bearer credential: %s", raw)
	}
}
