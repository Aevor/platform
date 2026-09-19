package repositories

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/ai"
	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

// analysesFixture exposes the analyses route backed by a clone fixture so read
// endpoints can be exercised exactly as a client would.
type analysesFixture struct {
	*cloneFixture
}

// newAnalysesFixture builds a clone fixture and registers the analyses route.
func newAnalysesFixture(t *testing.T, githubHandler http.HandlerFunc) *analysesFixture {
	t.Helper()

	fixture := newCloneFixture(t, githubHandler)

	handler := NewHandler(fixture.service)

	fixture.router.GET(
		"/repositories/:id/analyses",
		auth.RequireAuth(fixture.jwtManager),
		handler.ListAnalyses,
	)

	return &analysesFixture{cloneFixture: fixture}
}

// analysesCall performs GET /repositories/:id/analyses.
func analysesCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/repositories/"+selectedID+"/analyses", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

// seedAnalysis writes one analysis (plus insights) for a selected repository
// directly through the fake store, mirroring what AnalyzeRepositoryContent's
// persistAnalyzeResult writes.
func (f *cloneFixture) seedAnalysis(t *testing.T, selectedID uuid.UUID, query string, insights []ai.Insight) uuid.UUID {
	t.Helper()

	analysis := &RepositoryAnalysis{
		SelectedRepositoryID: selectedID,
		Query:                query,
		Summary:              "summary for: " + query,
		Status:               "analyzed",
		InsightsCount:        len(insights),
		AnalyzedAt:           time.Now(),
	}

	childInsights := make([]RepositoryAnalysisInsight, 0, len(insights))
	for _, insight := range insights {
		childInsights = append(childInsights, RepositoryAnalysisInsight{
			Type:       insight.Type,
			FilePath:   insight.FilePath,
			StartLine:  insight.StartLine,
			EndLine:    insight.EndLine,
			Message:    insight.Message,
			Confidence: insight.Confidence,
		})
	}

	if err := f.store.UpsertAnalysis(analysis, childInsights); err != nil {
		t.Fatalf("seed analysis: %v", err)
	}

	return analysis.ID
}

func TestListAnalyses_HandlerContract(t *testing.T) {
	fixture := newAnalysesFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := analysesCall(t, fixture.cloneFixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := analysesCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		// Seed an analysis on the OWNER's repository so the foreign context is
		// provably different data that must still be unreachable.
		fixture.seedAnalysis(t, cloneSelected, "how does auth work?", []ai.Insight{
			{Type: "overview", FilePath: "internal/auth/middleware.go", Message: "middleware verifies JWT", Confidence: 0.9},
		})

		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := analysesCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})
}

func TestListAnalyses_EmptyHistoryReturnsEmptyList(t *testing.T) {
	fixture := newAnalysesFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	recorder := analysesCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.String()

	if !strings.Contains(body, `"repository_id":"`+cloneSelected.String()+`"`) ||
		!strings.Contains(body, `"count":0`) ||
		!strings.Contains(body, `"analyses":[]`) {
		t.Errorf("unexpected empty-history body: %s", body)
	}
}

func TestListAnalyses_ReturnsOwnedHistoryWithInsights(t *testing.T) {
	fixture := newAnalysesFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	_ = fixture.seedAnalysis(t, cloneSelected, "first query", []ai.Insight{
		{Type: "overview", FilePath: "a.go", StartLine: 1, EndLine: 5, Message: "first insight", Confidence: 0.8},
	})
	_ = fixture.seedAnalysis(t, cloneSelected, "second query", []ai.Insight{
		{Type: "detail", FilePath: "b.go", StartLine: 2, EndLine: 9, Message: "second insight", Confidence: 0.95},
	})

	recorder := analysesCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload listAnalysesResponse

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() {
		t.Errorf("repository_id=%q, want %q", payload.RepositoryID, cloneSelected.String())
	}

	if payload.Count != 2 || len(payload.Analyses) != 2 {
		t.Fatalf("count=%d analyses=%d, want 2/2", payload.Count, len(payload.Analyses))
	}

	// Newest first: "second query" was seeded after "first query".
	if payload.Analyses[0].Query != "second query" || payload.Analyses[1].Query != "first query" {
		t.Errorf("ordering wrong: got %q then %q, want second then first",
			payload.Analyses[0].Query, payload.Analyses[1].Query)
	}

	for _, entry := range payload.Analyses {
		if entry.AnalysisID == uuid.Nil {
			t.Errorf("entry %q has nil analysis_id", entry.Query)
		}

		if !strings.Contains(entry.Summary, entry.Query) {
			t.Errorf("entry %q summary %q missing query text", entry.Query, entry.Summary)
		}

		if entry.Status != "analyzed" {
			t.Errorf("entry %q status=%q, want analyzed", entry.Query, entry.Status)
		}
	}

	second := payload.Analyses[0]

	if len(second.Insights) != 1 {
		t.Fatalf("second recall insights=%d, want 1", len(second.Insights))
	}

	insight := second.Insights[0]

	if insight.Type != "detail" || insight.FilePath != "b.go" || insight.StartLine != 2 ||
		insight.EndLine != 9 || insight.Message != "second insight" || insight.Confidence != 0.95 {
		t.Errorf("insight decoding wrong: %+v", insight)
	}

	// SECURITY: no raw AI output, no repository paths, no secrets, no GitHub
	// token material cross the boundary.
	body := recorder.Body.String()

	for _, leaked := range []string{
		"github_repository", "git_hub_access_token", "ghs_", "gitHubAccessToken",
		"clone_url", cloneTokenPlaintext, "workspaces",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q: %s", leaked, body)
		}
	}
}

func TestListAnalyses_FullAnalyzeFlowThenReadback(t *testing.T) {
	// Real end-to-end: run the bounded AI analysis (which persists via
	// persistAnalyzeResult), then read the durable history back through the
	// analyses endpoint on the same owned repository.
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

	// Index + analyze twice (two distinct queries) so history has two rows.
	indexRecorder := indexCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())
	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	for _, query := range []string{"explain Alpha", "describe the repo layout"} {
		recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{"query": query})
		if recorder.Code != http.StatusOK {
			t.Fatalf("analyze (%q) status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
	}

	// Re-asking the FIRST query again must refresh the SAME row (no growth).
	recorder := analyzeCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), map[string]string{"query": "explain Alpha"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-analyze status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// Register + read back the analysis history through the analyses route.
	handler := NewHandler(fixture.service)
	fixture.router.GET(
		"/repositories/:id/analyses",
		auth.RequireAuth(fixture.jwtManager),
		handler.ListAnalyses,
	)

	history := analysesCall(t, fixture.cloneFixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if history.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", history.Code, history.Body.String())
	}

	var payload listAnalysesResponse

	if err := json.Unmarshal(history.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode history: %v", err)
	}

	// Exactly two DISTINCT rows: re-asking the same query refreshed, not grew.
	if payload.Count != 2 {
		t.Fatalf("history count=%d, want 2 distinct queries (same-query re-ask must refresh)", payload.Count)
	}

	seen := map[string]bool{}
	for _, entry := range payload.Analyses {
		seen[entry.Query] = true
	}

	if !seen["explain Alpha"] || !seen["describe the repo layout"] {
		t.Errorf("history queries = %v, want explain Alpha and describe the repo layout", seen)
	}

	// Security: the history response never carries source content, repo paths,
	// the AI request content marker, or secrets.
	body := history.Body.String()

	for _, leaked := range []string{
		"SOURCE_MARKER", "func Alpha", dir, "SHOULD_NEVER_GO_TO_AI",
		cloneTokenPlaintext, "escape",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("history response leaks %q", leaked)
		}
	}
}
