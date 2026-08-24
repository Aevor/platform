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

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/workspace"
)

// chunkCall posts /repositories/:id/chunk.
func chunkCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/chunk", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestChunk_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := chunkCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) { // 18. isolation
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupted workspace is workspace_not_ready", func(t *testing.T) {
		junkDir, err := fixture.service.workspaces.Reset(cloneSelected)

		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(junkDir, "leftover.tmp"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		recorder := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready for corrupted dir", recorder.Code, recorder.Body.String())
		}
	})
}

func TestChunk_HappyPathMetadataOnly(t *testing.T) {
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":                   "package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"SECRET_SOURCE_MARKER\")\n}\n\nfunc Beta() {\n\tAlpha()\n}\n",
		"README.md":                 "# Demo\n\ndocumentation marker\n",
		"go.mod":                    "module demo\n",
		".github/workflows/ci.yml":  "on: push\n",
		".env":                      "SHOULD_NEVER_BE_CHUNKED=1",
		"node_modules/lib/index.js": "junk",
		"assets/logo.png":           "\x89PNG\r\n\x1a\n",
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

	recorder := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID  string `json:"repository_id"`
		TotalFiles    int    `json:"total_files"`
		FilesChunked  int    `json:"files_chunked"`
		EmptyFiles    int    `json:"empty_files"`
		TotalChunks   int    `json:"total_chunks"`
		TotalBytes    int64  `json:"total_bytes"`
		Truncated     bool   `json:"truncated"`
		SkippedSummry map[string]int
		Files         []struct {
			Path      string `json:"path"`
			Language  string `json:"language"`
			Chunks    int    `json:"chunks"`
			Bytes     int64  `json:"bytes"`
			Truncated bool   `json:"truncated"`
		} `json:"files"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "chunked" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// Only what filtering selected AND extraction extracted:
	// main.go, README.md, go.mod, ci.yml (4 files; symlink/.env/png/junk never enter).
	if payload.TotalFiles != 4 || payload.FilesChunked != 4 || payload.EmptyFiles != 0 {
		t.Errorf("counts wrong: total=%d chunked=%d empty=%d", payload.TotalFiles, payload.FilesChunked, payload.EmptyFiles)
	}

	if payload.Truncated {
		t.Error("small fixture must not be truncated")
	}

	summaries := map[string]struct {
		language string
		chunks   int
	}{}

	for _, summary := range payload.Files {
		summaries[summary.Path] = struct {
			language string
			chunks   int
		}{summary.Language, summary.Chunks}
	}

	if _, ok := summaries["main.go"]; !ok {
		t.Fatal("main.go missing from summaries")
	}

	if len(summaries) != 4 {
		t.Fatalf("summaries = %d, want 4 (%v)", len(summaries), payload.Files)
	}

	// Structural segmentation visible through per-file counts: main.go must
	// produce more than one chunk (package/import/functions are separate).
	if summaries["main.go"].chunks < 3 {
		t.Errorf("main.go chunks = %d, want >=3 from structural boundaries", summaries["main.go"].chunks)
	}

	if summaries["README.md"].chunks != 1 || summaries["go.mod"].chunks != 1 {
		t.Errorf("doc/config chunk counts wrong: %+v", summaries)
	}

	body := recorder.Body.String()

	// SECURITY: contents NEVER cross HTTP — not even a content key exists;
	// no source markers, secrets, or absolute paths leak.
	if strings.Contains(body, `"content"`) {
		t.Error("response carries a content field")
	}

	for _, leaked := range []string{dir, source, outside, "SECRET_SOURCE_MARKER", "TOP SECRET", "documentation marker", "SHOULD_NEVER_BE_CHUNKED", "left-pad", ".env", "node_modules", "logo.png"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}

	// Read-only guarantees hold end to end.
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "lib", "index.js")); err != nil {
		t.Errorf("ignored directory content was deleted: %v", err)
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}
}

func TestChunk_DeterministicResponses(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		"src/app.go": "package app\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(1)\n}\n",
		"docs.md":    "# Docs\n\nbody text\n",
	}

	for relative, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(relative))

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	second := chunkCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, first.Body.String())
	}

	if first.Body.String() != second.Body.String() {
		t.Errorf("responses differ between identical runs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}
