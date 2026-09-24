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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// issuePipelineFixture wires the service + issue-pipeline routes to a
// recording mock AI service. Response JSON for each of the three AI endpoints
// can be overridden per-test via overrideAIResponse before a call is made.
type issuePipelineFixture struct {
	*cloneFixture
	aiServer       *httptest.Server
	lock           sync.Mutex
	received       []*recordedIssueCall
	analyzeJSON    string
	proposeJSON    string
	generateJSON   string
	validationJSON string
}

type recordedIssueCall struct {
	Path       string
	Analyze    *ai.AnalyzeIssueRequest
	Propose    *ai.ProposeSolutionRequest
	Generate   *ai.GenerateChangesRequest
	Validation *ai.AnalyzeValidationFailureRequest
}

const (
	issueDefaultAnalyzeJSON    = `{"summary":"root cause found","root_cause":"the Alpha body mishandles X","affected_files":["main.go"],"status":"analyzed"}`
	issueDefaultProposeJSON    = `{"summary":"refactor Alpha","approach":"split the Alpha body into a helper","risks":["rename risk"],"status":"proposed"}`
	issueDefaultGenerateJSON   = `{"summary":"fix the Alpha flow","changes":[{"file_path":"main.go","operation":"modify","symbol":"Alpha","rationale":"fix the Alpha body","original_context":"func Alpha() {","proposed_content":"package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"FIXED\")\n}\n"}],"tests":[{"file_path":"alpha_extra_test.go","operation":"add","proposed_content":"package demo\n\nimport \"testing\"\n\nfunc TestAlphaExtra(t *testing.T) {\n\tAlpha()\n}\n","rationale":"cover the fix"}],"assumptions":["the marker is the bug"],"uncertainty":"moderate","status":"generated"}`
	issueDefaultValidationJSON = `{"summary":"analysis of the validation failure","root_cause":"sample root cause","suggestions":["sample suggestion"],"status":"analyzed"}`
)

func newIssuePipelineFixture(t *testing.T, githubHandler http.HandlerFunc) *issuePipelineFixture {
	t.Helper()

	fixture := newCloneFixture(t, githubHandler)

	f := &issuePipelineFixture{
		cloneFixture: fixture,
	}

	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := &recordedIssueCall{Path: r.URL.Path}
		var defaultJSON string

		switch r.URL.Path {
		case "/v1/analyze-issue":
			call.Analyze = &ai.AnalyzeIssueRequest{}
			if err := json.NewDecoder(r.Body).Decode(call.Analyze); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			defaultJSON = issueDefaultAnalyzeJSON
		case "/v1/propose-solution":
			call.Propose = &ai.ProposeSolutionRequest{}
			if err := json.NewDecoder(r.Body).Decode(call.Propose); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			defaultJSON = issueDefaultProposeJSON
		case "/v1/generate-changes":
			call.Generate = &ai.GenerateChangesRequest{}
			if err := json.NewDecoder(r.Body).Decode(call.Generate); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			defaultJSON = issueDefaultGenerateJSON
		case "/v1/analyze-validation":
			call.Validation = &ai.AnalyzeValidationFailureRequest{}
			if err := json.NewDecoder(r.Body).Decode(call.Validation); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			defaultJSON = issueDefaultValidationJSON
		default:
			http.NotFound(w, r)
			return
		}

		f.lock.Lock()
		f.received = append(f.received, call)
		override := ""
		switch r.URL.Path {
		case "/v1/analyze-issue":
			override = f.analyzeJSON
		case "/v1/propose-solution":
			override = f.proposeJSON
		case "/v1/generate-changes":
			override = f.generateJSON
		case "/v1/analyze-validation":
			override = f.validationJSON
		}
		body := defaultJSON
		if override != "" {
			body = override
		}
		f.lock.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(aiServer.Close)

	client := ai.NewClient(nil, ai.WithBaseURL(aiServer.URL))
	f.service.aiClient = client

	handler := NewHandler(f.service)

	f.router.POST(
		"/repositories/:id/issues/:issueID/analyze",
		auth.RequireAuth(f.jwtManager),
		handler.AnalyzeIssue,
	)
	f.router.POST(
		"/repositories/:id/issues/:issueID/propose",
		auth.RequireAuth(f.jwtManager),
		handler.ProposeSolution,
	)
	f.router.POST(
		"/repositories/:id/issues/:issueID/generate-changes",
		auth.RequireAuth(f.jwtManager),
		handler.GenerateChanges,
	)
	f.router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/approve",
		auth.RequireAuth(f.jwtManager),
		handler.ApproveChangeSet,
	)
	f.router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/apply",
		auth.RequireAuth(f.jwtManager),
		handler.ApplyChangeSet,
	)
	f.router.POST(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/validate",
		auth.RequireAuth(f.jwtManager),
		handler.ValidateChangeSet,
	)
	f.router.GET(
		"/repositories/:id/issues/:issueID/changes/:changeSetID/validation-plan",
		auth.RequireAuth(f.jwtManager),
		handler.ValidationPlan,
	)

	f.aiServer = aiServer
	return f
}

// overrideAIResponse lets a test replace the JSON body served by the mock AI
// service for one endpoint path.
func (f *issuePipelineFixture) overrideAIResponse(path, jsonBody string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	switch path {
	case "/v1/analyze-issue":
		f.analyzeJSON = jsonBody
	case "/v1/propose-solution":
		f.proposeJSON = jsonBody
	case "/v1/generate-changes":
		f.generateJSON = jsonBody
	case "/v1/analyze-validation":
		f.validationJSON = jsonBody
	}
}

// newIssuePipelineFileFixture builds the fixture over a real local Git
// repository so CloneRepository exercises genuine go-git file:// cloning and
// GenerateChanges validates the actual workspace content.
func newIssuePipelineFileFixture(t *testing.T) *issuePipelineFixture {
	t.Helper()

	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))

	return newIssuePipelineFixture(t, githubRepoResponse(t, "file://"+source))
}

func (f *issuePipelineFixture) calls() []*recordedIssueCall {
	f.lock.Lock()
	defer f.lock.Unlock()

	out := make([]*recordedIssueCall, len(f.received))
	copy(out, f.received)

	return out
}

// issuePipelineCall posts to an arbitrary issue-pipeline path.
func issuePipelineCall(t *testing.T, fixture *cloneFixture, jwtToken string, path string) *httptest.ResponseRecorder {
	t.Helper()

	return issuePipelineRequest(t, fixture, jwtToken, http.MethodPost, path)
}

// issuePipelineGET gets an arbitrary read-only issue-pipeline path.
func issuePipelineGET(t *testing.T, fixture *cloneFixture, jwtToken string, path string) *httptest.ResponseRecorder {
	t.Helper()

	return issuePipelineRequest(t, fixture, jwtToken, http.MethodGet, path)
}

func issuePipelineRequest(t *testing.T, fixture *cloneFixture, jwtToken string, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, path, nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

// seedWorkspace clones the configured local repo, writes source files, and
// rebuilds the metadata index so context selection finds the seeded source.
// Mirrors real usage (clone then index → analyze).
func (f *issuePipelineFixture) seedWorkspace(t *testing.T) string {
	t.Helper()

	f.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := f.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := f.service.workspaces.Dir(cloneSelected)

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(
		"package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"SOURCE_MARKER\")\n}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	sourceTest := filepath.Join(dir, "main_test.go")
	if err := os.MkdirAll(filepath.Dir(sourceTest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceTest, []byte(
		"package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {\n\tAlpha()\n}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.IndexRepositoryContent(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	return dir
}

// seedIssue inserts one owned issue and returns its Aevor issue ID.
func (f *issuePipelineFixture) seedIssue(t *testing.T, title string) uuid.UUID {
	t.Helper()

	if err := f.store.UpsertIssues(cloneSelected, []RepositoryIssue{
		{
			GithubIssueID: 9001,
			Number:        42,
			Title:         title,
			State:         "open",
			AuthorLogin:   "octocat",
		},
	}); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	return f.store.issues[issueKey{cloneSelected, 9001}].ID
}

func (f *issuePipelineFixture) seedAnalysis(t *testing.T, issueID uuid.UUID) uuid.UUID {
	t.Helper()

	analysis := &RepositoryIssueAnalysis{
		SelectedRepositoryID: cloneSelected,
		RepositoryIssueID:    issueID,
		Summary:              "root cause found",
		RootCause:            "the Alpha body mishandles X",
		AffectedFiles:        "main.go",
		Status:               "analyzed",
		AnalyzedAt:           time.Now(),
	}

	if err := f.store.UpsertIssueAnalysis(analysis); err != nil {
		t.Fatalf("seed analysis: %v", err)
	}

	return analysis.ID
}

func (f *issuePipelineFixture) seedProposal(t *testing.T, issueID uuid.UUID) uuid.UUID {
	t.Helper()

	proposal := &RepositorySolutionProposal{
		SelectedRepositoryID: cloneSelected,
		RepositoryIssueID:    issueID,
		Summary:              "refactor Alpha",
		Approach:             "split the Alpha body into a helper",
		Risks:                "rename risk\nsecond risk",
		Status:               "proposed",
		ProposedAt:           time.Now(),
	}

	if err := f.store.UpsertSolutionProposal(proposal); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}

	return proposal.ID
}

func TestIssuePipeline_HandlerContract(t *testing.T) {
	fixture := newIssuePipelineFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")

	analyzePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"
	proposePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/propose"
	generatePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		for _, path := range []string{analyzePath, proposePath, generatePath} {
			recorder := issuePipelineCall(t, fixture.cloneFixture, "", path)
			if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
				t.Errorf("%s: status=%d body=%s, want uniform unauthorized", path, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("malformed ids are invalid_request", func(t *testing.T) {
		for _, path := range []string{
			"/repositories/not-a-uuid/issues/" + issueID.String() + "/generate-changes",
			"/repositories/" + cloneSelected.String() + "/issues/not-a-uuid/generate-changes",
		} {
			recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)
			if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
				t.Errorf("%s: status=%d body=%s, want invalid_request", path, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("foreign and unknown repositories are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			path := "/repositories/" + target.String() + "/issues/" + issueID.String() + "/generate-changes"
			recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

			if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("issue not owned by the repository is opaque 404", func(t *testing.T) {
		foreignIssueID := uuid.MustParse("cccccccc-0f00-0000-0000-000000000001")
		path := "/repositories/" + cloneSelected.String() + "/issues/" + foreignIssueID.String() + "/generate-changes"
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

		if recorder.Code != http.StatusNotFound || strings.TrimSpace(recorder.Body.String()) != `{"error":"issue_not_found"}` {
			t.Errorf("status=%d body=%s, want issue_not_found", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("propose without analysis is issue_analysis_missing", func(t *testing.T) {
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), proposePath)

		if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"issue_analysis_missing"}` {
			t.Errorf("status=%d body=%s, want issue_analysis_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("generate without analysis is issue_analysis_missing", func(t *testing.T) {
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"issue_analysis_missing"}` {
			t.Errorf("status=%d body=%s, want issue_analysis_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("generate with analysis but no proposal is solution_proposal_missing", func(t *testing.T) {
		fixture.seedAnalysis(t, issueID)
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"solution_proposal_missing"}` {
			t.Errorf("status=%d body=%s, want solution_proposal_missing", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("generate without a ready workspace is workspace_not_ready", func(t *testing.T) {
		fixture.seedProposal(t, issueID)
		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusConflict || strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})
}

func TestAnalyzeIssue_HappyPath(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	calls := fixture.calls()
	if len(calls) != 1 {
		t.Fatalf("ai service received %d calls, want 1", len(calls))
	}

	request := calls[0].Analyze
	if request == nil {
		t.Fatal("no analyze-issue request captured")
	}
	if request.RepositoryID != cloneSelected.String() || request.RepositoryName != "hello-world" {
		t.Errorf("identity wrong: id=%s name=%s", request.RepositoryID, request.RepositoryName)
	}
	if request.Issue.Number != 42 || request.Issue.Title != "Fix the Alpha flow" || request.Issue.State != "open" {
		t.Errorf("issue info wrong: %+v", request.Issue)
	}

	joined := ""
	for _, chunk := range request.ContextChunks {
		joined += chunk.Content
	}
	if !strings.Contains(joined, "SOURCE_MARKER") || !strings.Contains(joined, "func Alpha") {
		t.Error("analysis context missing actual source content")
	}

	if !strings.Contains(recorder.Body.String(), `"summary":"root cause found"`) {
		t.Errorf("response missing analysis summary: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "SOURCE_MARKER") {
		t.Errorf("response leaks source content: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), dir) {
		t.Errorf("response leaks workspace path: %s", recorder.Body.String())
	}

	stored, err := fixture.store.GetIssueAnalysis(cloneSelected, issueID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Summary != "root cause found" || stored.RootCause == "" {
		t.Errorf("issue analysis not persisted durably: %+v", stored)
	}
}

func TestProposeSolution_HappyPath(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	analysisID := fixture.seedAnalysis(t, issueID)

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/propose"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	calls := fixture.calls()
	if len(calls) != 1 {
		t.Fatalf("ai service received %d calls, want 1", len(calls))
	}

	request := calls[0].Propose
	if request == nil {
		t.Fatal("no propose-solution request captured")
	}
	if request.IssueAnalysis.Summary != "root cause found" || request.IssueAnalysis.RootCause == "" {
		t.Errorf("propose request missing persisted analysis: %+v", request.IssueAnalysis)
	}
	if request.Issue.Title != "Fix the Alpha flow" {
		t.Errorf("issue info wrong: %+v", request.Issue)
	}

	if !strings.Contains(recorder.Body.String(), `"summary":"refactor Alpha"`) || !strings.Contains(recorder.Body.String(), `"approach":"split the Alpha body into a helper"`) {
		t.Errorf("response missing proposal fields: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), dir) {
		t.Errorf("response leaks workspace path: %s", recorder.Body.String())
	}

	stored, err := fixture.store.GetSolutionProposal(cloneSelected, issueID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Summary != "refactor Alpha" || stored.IssueAnalysisID != analysisID {
		t.Errorf("solution proposal not persisted durably or analysis link wrong: %+v", stored)
	}
}

func TestGenerateChanges_HappyPath(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	calls := fixture.calls()
	if len(calls) != 1 {
		t.Fatalf("ai service received %d calls, want 1", len(calls))
	}

	request := calls[0].Generate
	if request == nil {
		t.Fatal("no generate-changes request captured")
	}
	if request.Issue.Title != "Fix the Alpha flow" {
		t.Errorf("issue info wrong: %+v", request.Issue)
	}
	if request.IssueAnalysis.Summary != "root cause found" {
		t.Errorf("generate request missing analysis: %+v", request.IssueAnalysis)
	}
	if request.SolutionProposal.Summary != "refactor Alpha" {
		t.Errorf("generate request missing proposal: %+v", request.SolutionProposal)
	}

	joined := ""
	for _, chunk := range request.ContextChunks {
		joined += chunk.Content
	}
	if !strings.Contains(joined, "SOURCE_MARKER") {
		t.Error("generate request context missing actual source content")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"summary":"fix the Alpha flow"`) {
		t.Errorf("response missing change summary: %s", body)
	}
	if !strings.Contains(body, `"file_path":"main.go"`) || !strings.Contains(body, `"operation":"modify"`) {
		t.Errorf("response missing structured change: %s", body)
	}

	// THE ACTUAL DIFF: computed from the workspace original vs proposed. Decode
	// so the diff is inspected with real tabs/quotes rather than the JSON
	// escaping of the raw body.
	var decoded generateChangesResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(decoded.Changes[0].Diff, "--- a/main.go") || !strings.Contains(decoded.Changes[0].Diff, "+++ b/main.go") {
		t.Errorf("response missing actual diff headers: %s", decoded.Changes[0].Diff)
	}
	if !strings.Contains(decoded.Changes[0].Diff, "+\tfmt.Println(\"FIXED\")") {
		t.Errorf("response diff missing proposed content: %s", decoded.Changes[0].Diff)
	}
	if !strings.Contains(decoded.Changes[0].Diff, "-\tfmt.Println(\"SOURCE_MARKER\")") {
		t.Errorf("response diff missing original content removal: %s", decoded.Changes[0].Diff)
	}
	if len(decoded.Tests) == 0 || !strings.Contains(body, `"alpha_extra_test.go"`) {
		t.Errorf("response missing proposed test file: %s", body)
	}
	if !strings.Contains(body, `"assumptions"`) || !strings.Contains(body, `"uncertainty":"moderate"`) {
		t.Errorf("response missing assumptions/uncertainty: %s", body)
	}

	// ORIGINAL WORKSPACE PRESERVATION: the workspace file must be byte-identical.
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "FIXED") {
		t.Errorf("workspace main.go was modified despite in-memory apply:\n%s", string(data))
	}
	if !strings.Contains(string(data), "SOURCE_MARKER") {
		t.Errorf("workspace main.go altered:\n%s", string(data))
	}

	// THE DIFF IS REVIEWABLE OUTPUT BY DESIGN (it legitimately contains the
	// changed lines). What must never leak is the absolute workspace path.
	if strings.Contains(body, dir) {
		t.Errorf("HTTP response leaks workspace path: %s", body)
	}
}

func TestGenerateChanges_RejectsInvalidChangeSet(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)

	original, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	generatePath := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"

	assertWorkspaceUntouched := func(t *testing.T) {
		t.Helper()
		data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
		if string(data) != string(original) {
			t.Errorf("workspace changed despite rejection:\n%s", string(data))
		}
	}

	t.Run("traversal path is unprocessable", func(t *testing.T) {
		fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"bad","changes":[{"file_path":"../escape.go","operation":"modify","proposed_content":"x\n"}],"tests":[],"status":"generated"}`)

		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusUnprocessableEntity || strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_change_set"}` {
			t.Errorf("status=%d body=%s, want invalid_change_set", recorder.Code, recorder.Body.String())
		}
		assertWorkspaceUntouched(t)
	})

	t.Run("context mismatch is unprocessable", func(t *testing.T) {
		fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"bad","changes":[{"file_path":"main.go","operation":"modify","original_context":"THIS DOES NOT EXIST","proposed_content":"y\n"}],"tests":[],"status":"generated"}`)

		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("status=%d body=%s, want invalid_change_set", recorder.Code, recorder.Body.String())
		}
		assertWorkspaceUntouched(t)
	})

	t.Run("unsupported operation is unprocessable", func(t *testing.T) {
		fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"bad","changes":[{"file_path":"main.go","operation":"overwrite","proposed_content":"y\n"}],"tests":[],"status":"generated"}`)

		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("status=%d body=%s, want invalid_change_set", recorder.Code, recorder.Body.String())
		}
		assertWorkspaceUntouched(t)
	})

	t.Run("modification of a phantom file is unprocessable", func(t *testing.T) {
		fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"bad","changes":[{"file_path":"internal/phantom/never_seen.go","operation":"modify","proposed_content":"y\n"}],"tests":[],"status":"generated"}`)

		recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), generatePath)

		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("status=%d body=%s, want invalid_change_set", recorder.Code, recorder.Body.String())
		}
		assertWorkspaceUntouched(t)
	})
}

func TestIssuePipeline_FailsClosedWhenAIClientNil(t *testing.T) {
	fixture := newIssuePipelineFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)
	fixture.service.aiClient = nil

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusInternalServerError ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"internal"}` {
		t.Errorf("status=%d body=%s, want fail-closed internal", recorder.Code, recorder.Body.String())
	}
}

func TestGenerateChanges_AIServiceUnavailable(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)

	// Nothing listens on port 1: the AI unreachability path maps to
	// ai.ErrUnavailable, which must surface as a 503, not a 500.
	fixture.service.aiClient = ai.NewClient(nil, ai.WithBaseURL("http://127.0.0.1:1"))

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusServiceUnavailable ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"ai_service_unavailable"}` {
		t.Errorf("status=%d body=%s, want 503 ai_service_unavailable", recorder.Code, recorder.Body.String())
	}
}

func TestGenerateChanges_SymlinkInWorkspaceRejected(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	dir := fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.seedAnalysis(t, issueID)
	fixture.seedProposal(t, issueID)

	outsideSecret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("TOP SECRET SERVER FILE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(dir, "escape.go")); err != nil {
		t.Fatal(err)
	}

	fixture.overrideAIResponse("/v1/generate-changes", `{"summary":"bad","changes":[{"file_path":"escape.go","operation":"modify","proposed_content":"owned\n"}],"tests":[],"status":"generated"}`)

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/generate-changes"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d body=%s, want invalid_change_set for symlink target", recorder.Code, recorder.Body.String())
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}
}

func TestIssuePipeline_StoreFailCloses(t *testing.T) {
	fixture := newIssuePipelineFileFixture(t)
	fixture.seedWorkspace(t)
	issueID := fixture.seedIssue(t, "Fix the Alpha flow")
	fixture.store.upsertErr = fmt.Errorf("db down")

	path := "/repositories/" + cloneSelected.String() + "/issues/" + issueID.String() + "/analyze"
	recorder := issuePipelineCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), path)

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status=%d body=%s, want internal on store failure", recorder.Code, recorder.Body.String())
	}
}
