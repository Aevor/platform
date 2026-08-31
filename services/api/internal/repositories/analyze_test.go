package repositories

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// analyzeFixture wires the service + analyze route plus a local mock AI
// service that captures every request it receives. The real workspace cloner
// is left in place so happy-path tests can seed an actual cloned workspace.
type analyzeFixture struct {
	*cloneFixture
	aiServer *httptest.Server
	aiClient *ai.Client
	lock     *sync.Mutex
	received *[]*ai.AnalyzeRequest
}

// newAnalyzeFixture builds a cloneFixture, attaches a recording mock AI
// service to the analyze route, and records every AnalyzeRequest it receives.
func newAnalyzeFixture(t *testing.T, githubHandler http.HandlerFunc) *analyzeFixture {
	t.Helper()

	fixture := newCloneFixture(t, githubHandler)

	lock := &sync.Mutex{}
	var received []*ai.AnalyzeRequest

	aiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ai.AnalyzeRequest

		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		lock.Lock()
		received = append(received, &request)
		lock.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"summary":"analyzed","insights":[],"status":"analyzed"}`))
	}))
	t.Cleanup(aiServer.Close)

	client := ai.NewClient(nil, ai.WithBaseURL(aiServer.URL))
	fixture.service.aiClient = client

	handler := NewHandler(fixture.service)

	fixture.router.POST(
		"/repositories/:id/index",
		auth.RequireAuth(fixture.jwtManager),
		handler.Index,
	)
	fixture.router.POST(
		"/repositories/:id/analyze",
		auth.RequireAuth(fixture.jwtManager),
		handler.Analyze,
	)

	return &analyzeFixture{
		cloneFixture: fixture,
		aiServer:     aiServer,
		aiClient:     client,
		lock:         lock,
		received:     &received,
	}
}

// requests returns a snapshot of the AnalyzeRequests captured by the mock AI
// service.
func (f *analyzeFixture) requests() []*ai.AnalyzeRequest {
	f.lock.Lock()
	defer f.lock.Unlock()

	out := make([]*ai.AnalyzeRequest, len(*f.received))
	copy(out, *f.received)

	return out
}

// analyzeCall posts /repositories/:id/analyze with a JSON query body.
func analyzeCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal analyze body: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/analyze", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestAnalyze_HandlerContract(t *testing.T) {
	fixture := newAnalyzeFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := analyzeCall(t, fixture.cloneFixture, "", cloneSelected.String(), map[string]string{"query": "how does auth work?"})

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid", map[string]string{"query": "hi"})

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("missing body is invalid_request", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/repositories/"+cloneSelected.String()+"/analyze", nil)
		request.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, cloneUserID))
		recorder := httptest.NewRecorder()
		fixture.router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("empty query is query_required", func(t *testing.T) {
		recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{"query": "   "})

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"query_required"}` {
			t.Errorf("status=%d body=%s, want query_required", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), target.String(), map[string]string{"query": "hi"})

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{"query": "hi"})

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})
}

func TestAnalyze_HappyPathSendsContentToAIService(t *testing.T) {
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newAnalyzeFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":      "package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"SOURCE_MARKER\")\n}\n",
		"main_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {\n\tAlpha()\n}\n",
		"README.md":    "# Demo\n\ndocumentation marker\n",
		"go.mod":       "module demo\n",
		".env":         "SHOULD_NEVER_GO_TO_AI=1",
	}

	for relative, content := range seeded {
		full := filepath.Join(dir, relative)

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	outsideSecret := filepath.Join(outside, "secret.txt")

	if err := os.WriteFile(outsideSecret, []byte("TOP SECRET SERVER FILE"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outsideSecret, filepath.Join(dir, "escape.go")); err != nil {
		t.Fatal(err)
	}

	// Index first (mirrors real usage) so the metadata index holds records.
	indexRecorder := indexCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{
		"query": "explain the Alpha function",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// The AI service received exactly one request.
	requests := fixture.requests()

	if len(requests) != 1 {
		t.Fatalf("ai service received %d requests, want 1", len(requests))
	}

	request := requests[0]

	if request.RepositoryID != cloneSelected.String() || request.RepositoryName != "hello-world" {
		t.Errorf("request identity wrong: id=%s name=%s", request.RepositoryID, request.RepositoryName)
	}

	if request.Query != "explain the Alpha function" {
		t.Errorf("query wrong: %q", request.Query)
	}

	// CONTENT ENRICHMENT: the chunks sent to the AI service carry the actual
	// bounded source content, not just metadata.
	joined := ""
	for _, chunk := range request.ContextChunks {
		if chunk.Content == "" {
			t.Errorf("chunk %s (file %s) has empty content, want source content", chunk.ID, chunk.FilePath)
		}
		joined += chunk.Content
	}

	if !strings.Contains(joined, "SOURCE_MARKER") {
		t.Error("AI request context does not include actual source content (SOURCE_MARKER)")
	}

	if !strings.Contains(joined, "func Alpha") {
		t.Error("AI request context does not include the Alpha function source")
	}

	// SECURITY: secrets and symlinked server files never reach the AI service.
	if strings.Contains(joined, "SHOULD_NEVER_GO_TO_AI") {
		t.Error("AI request context leaks .env secret")
	}

	if strings.Contains(joined, "TOP SECRET") {
		t.Error("AI request context leaks symlinked server secret")
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}

	// HTTP response carries the structured result but never source content.
	body := recorder.Body.String()

	if !strings.Contains(body, `"summary":"analyzed"`) || !strings.Contains(body, `"status":"analyzed"`) {
		t.Errorf("response missing analyzed summary/status: %s", body)
	}

	for _, leaked := range []string{dir, source, "SOURCE_MARKER", "func Alpha", "TOP SECRET", "SHOULD_NEVER_GO_TO_AI", "escape"} {
		if strings.Contains(body, leaked) {
			t.Errorf("HTTP response leaks %q", leaked)
		}
	}
}

func TestAnalyze_FailsClosedWhenAIClientNil(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))
	fixture.service.aiClient = nil

	handler := NewHandler(fixture.service)

	fixture.router.POST(
		"/repositories/:id/analyze",
		auth.RequireAuth(fixture.jwtManager),
		handler.Analyze,
	)

	recorder := analyzeCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{"query": "hi"})

	if recorder.Code != http.StatusInternalServerError ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"internal"}` {
		t.Errorf("status=%d body=%s, want fail-closed internal", recorder.Code, recorder.Body.String())
	}
}
