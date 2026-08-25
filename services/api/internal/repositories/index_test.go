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
	"testing"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/auth"
	"github.com/Aevor/platform/services/api/internal/workspace"
)

func newIndexFixture(t *testing.T, githubHandler http.HandlerFunc) *cloneFixture {
	t.Helper()

	fixture := newCloneFixture(t, githubHandler)

	handler := NewHandler(fixture.service)

	fixture.router.POST(
		"/repositories/:id/index",
		auth.RequireAuth(fixture.jwtManager),
		handler.Index,
	)
	fixture.router.GET(
		"/repositories/:id/index/files",
		auth.RequireAuth(fixture.jwtManager),
		handler.IndexedFiles,
	)
	fixture.router.POST(
		"/repositories/:id/index/lookup",
		auth.RequireAuth(fixture.jwtManager),
		handler.LookupIndexed,
	)

	return fixture
}

// indexCall posts /repositories/:id/index.
func indexCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/index", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

// indexedFilesCall gets /repositories/:id/index/files.
func indexedFilesCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/repositories/"+selectedID+"/index/files", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

// lookupIndexedCall posts /repositories/:id/index/lookup with a JSON body.
func lookupIndexedCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal lookup body: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/index/lookup", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestIndex_HandlerContract(t *testing.T) {
	fixture := newIndexFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := indexCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})
}

func TestIndex_HappyPathMetadataOnly(t *testing.T) {
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":                  "package demo\n\nimport \"fmt\"\n\nfunc Alpha() {\n\tfmt.Println(\"SOURCE_MARKER\")\n}\n",
		"main_test.go":             "package demo\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {\n\tAlpha()\n}\n",
		"README.md":                "# Demo\n\ndocumentation marker\n",
		"go.mod":                   "module demo\n",
		".github/workflows/ci.yml": "on: push\n",
		".env":                     "SHOULD_NEVER_BE_INDEXED=1",
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

	recorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID string `json:"repository_id"`
		Files        int    `json:"files"`
		Chunks       int    `json:"chunks"`
		Status       string `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "indexed" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// Only filtering-selected files are represented; the symlink, .env and
	// binary junk never enter the pipeline.
	if payload.Files != 5 || payload.Chunks == 0 {
		t.Errorf("counts wrong: files=%d chunks=%d", payload.Files, payload.Chunks)
	}

	body := recorder.Body.String()

	// SECURITY: contents NEVER cross HTTP — no markers, secrets, or
	// absolute paths leak.
	if strings.Contains(body, `"content"`) {
		t.Error("response carries a content field")
	}

	for _, leaked := range []string{dir, source, outside, "SOURCE_MARKER", "TOP SECRET", "documentation marker", "SHOULD_NEVER_BE_INDEXED", ".env", "escape"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}
}

func TestIndex_IdempotentAcrossReplacements(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	files := map[string]string{
		"src/app.go":      "package app\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(1)\n}\n",
		"src/app_test.go": "package app\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) {\n\tRun()\n}\n",
		"docs.md":         "# Docs\n\nbody text\n",
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

	first := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	second := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}

	if first.Body.String() != second.Body.String() {
		t.Errorf("responses differ between identical runs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

func TestIndexedFiles_HandlerContract(t *testing.T) {
	fixture := newIndexFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := indexedFilesCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := indexedFilesCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := indexedFilesCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})
}

func TestIndexedFiles_EmptyIndexReturnsEmptyList(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	recorder := indexedFilesCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID   string   `json:"repository_id"`
		Files          []string `json:"files"`
		Count          int      `json:"count"`
		FilesTruncated bool     `json:"files_truncated"`
		Status         string   `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "ok" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	if payload.Count != 0 || len(payload.Files) != 0 {
		t.Errorf("empty index: count=%d files=%d, want 0", payload.Count, len(payload.Files))
	}

	if payload.FilesTruncated {
		t.Error("empty index reports truncated")
	}
}

func TestIndexedFiles_HappyPathAfterIndex(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":       "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n",
		"main_test.go":  "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {\n\tmain()\n}\n",
		"README.md":     "# Demo\n\nreadme\n",
		"go.mod":        "module demo\n",
		".env":          "SECRET=1",
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

	// Index first.
	indexRecorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	// Then query files.
	filesRecorder := indexedFilesCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if filesRecorder.Code != http.StatusOK {
		t.Fatalf("files status=%d body=%s", filesRecorder.Code, filesRecorder.Body.String())
	}

	var payload struct {
		RepositoryID   string   `json:"repository_id"`
		Files          []string `json:"files"`
		Count          int      `json:"count"`
		FilesTruncated bool     `json:"files_truncated"`
		Status         string   `json:"status"`
	}

	if err := json.Unmarshal(filesRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "ok" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// Only filtering-selected files appear: main.go, main_test.go,
	// README.md, go.mod. .env is excluded.
	if payload.Count != 4 || len(payload.Files) != 4 {
		t.Errorf("file count wrong: count=%d len=%d, want 4", payload.Count, len(payload.Files))
	}

	if payload.FilesTruncated {
		t.Error("unexpected truncation")
	}

	for _, want := range []string{"main.go", "main_test.go", "README.md", "go.mod"} {
		found := false
		for _, f := range payload.Files {
			if f == want {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("expected file %q not in indexed files: %v", want, payload.Files)
		}
	}

	body := filesRecorder.Body.String()

	// SECURITY: no absolute paths, no secrets.
	for _, leaked := range []string{dir, "SECRET", ".env"} {
		if strings.Contains(body, leaked) {
			t.Errorf("files response leaks %q", leaked)
		}
	}
}

func TestLookupIndexed_HandlerContract(t *testing.T) {
	fixture := newIndexFixture(t, githubRepoResponse(t, "https://github.com/octocat/hello-world.git"))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := lookupIndexedCall(t, fixture, "", cloneSelected.String(), map[string]string{})

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid", map[string]string{})

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String(), map[string]string{})

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing body is invalid_request", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/repositories/"+cloneSelected.String()+"/index/lookup", nil)
		request.Header.Set("Authorization", "Bearer "+fixture.tokenFor(t, cloneUserID))
		recorder := httptest.NewRecorder()
		fixture.router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})
}

func TestLookupIndexed_EmptyQueryReturnsAll(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":      "package main\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(1)\n}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) {\n\tRun()\n}\n",
		"docs.md":      "# Docs\n\nbody\n",
		"go.mod":       "module demo\n",
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

	// Index first.
	indexRecorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	// Empty query body → all records.
	lookupRecorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), struct{}{})

	if lookupRecorder.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", lookupRecorder.Code, lookupRecorder.Body.String())
	}

	var payload struct {
		RepositoryID     string `json:"repository_id"`
		Count            int    `json:"count"`
		RecordsTruncated bool   `json:"records_truncated"`
		Status           string `json:"status"`
		Records          []struct {
			ID          string  `json:"id"`
			FilePath    string  `json:"file_path"`
			Directory   string  `json:"directory"`
			Extension   string  `json:"extension"`
			FileRole    string  `json:"file_role"`
			Language    string  `json:"language"`
			ChunkIndex  int     `json:"chunk_index"`
			StartLine   int     `json:"start_line"`
			EndLine     int     `json:"end_line"`
			ContentHash string  `json:"content_hash"`
			SymbolName  *string `json:"symbol_name"`
			SymbolType  string  `json:"symbol_type"`
		} `json:"records"`
	}

	if err := json.Unmarshal(lookupRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "ok" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// 4 filtered files → at least 4 records (Go files may produce multiple chunks).
	if payload.Count < 4 || len(payload.Records) < 4 {
		t.Errorf("record count wrong: count=%d records=%d, want >= 4", payload.Count, len(payload.Records))
	}

	if payload.Count != len(payload.Records) {
		t.Errorf("count mismatch: count=%d records=%d", payload.Count, len(payload.Records))
	}

	// SECURITY: no content field, no absolute paths, no secrets.
	body := lookupRecorder.Body.String()

	if strings.Contains(body, `"content"`) {
		t.Error("response carries a content field")
	}

	for _, leaked := range []string{dir, source, "SOURCE_MARKER", "TOP SECRET"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}
}

func TestLookupIndexed_FilteredQueryReturnsSubset(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go":      "package main\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(1)\n}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) {\n\tRun()\n}\n",
		"docs.md":      "# Docs\n\nbody\n",
		"go.mod":       "module demo\n",
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

	// Index first.
	indexRecorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	// Filter by file_role = test → only main_test.go.
	lookupRecorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), struct {
		FileRole string `json:"file_role"`
	}{
		FileRole: "test",
	})

	if lookupRecorder.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", lookupRecorder.Code, lookupRecorder.Body.String())
	}

	var payload struct {
		Count   int `json:"count"`
		Records []struct {
			FilePath string `json:"file_path"`
			FileRole string `json:"file_role"`
		} `json:"records"`
	}

	if err := json.Unmarshal(lookupRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.Count < 1 || len(payload.Records) < 1 {
		t.Fatalf("filtered count wrong: count=%d records=%d, want >= 1", payload.Count, len(payload.Records))
	}

	for _, record := range payload.Records {
		if record.FileRole != "test" {
			t.Errorf("filtered record role=%s, want test", record.FileRole)
		}
	}
}

func TestLookupIndexed_PathPrefixFilter(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"src/app.go":      "package app\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(1)\n}\n",
		"src/app_test.go": "package app\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) {\n\tRun()\n}\n",
		"cmd/main.go":     "package main\n\nimport \"fmt\"\n\nfunc Main() {\n\tfmt.Println(2)\n}\n",
		"go.mod":          "module demo\n",
	}

	for relative, content := range seeded {
		full := filepath.Join(dir, filepath.FromSlash(relative))

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Index first.
	indexRecorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	// Filter by path_prefix = "src/" → only src/app.go, src/app_test.go.
	lookupRecorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), struct {
		PathPrefix string `json:"path_prefix"`
	}{
		PathPrefix: "src/",
	})

	if lookupRecorder.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", lookupRecorder.Code, lookupRecorder.Body.String())
	}

	var payload struct {
		Count   int `json:"count"`
		Records []struct {
			FilePath string `json:"file_path"`
		} `json:"records"`
	}

	if err := json.Unmarshal(lookupRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.Count < 2 || len(payload.Records) < 2 {
		t.Fatalf("filtered count wrong: count=%d records=%d, want >= 2", payload.Count, len(payload.Records))
	}

	for _, record := range payload.Records {
		if !strings.HasPrefix(record.FilePath, "src/") {
			t.Errorf("record path %q does not have prefix src/", record.FilePath)
		}
	}
}

func TestLookupIndexed_SecurityMetadataOnly(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newIndexFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc Run() {\n\tfmt.Println(\"SECRET_VALUE_42\")\n}\n",
		"go.mod":  "module demo\n",
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

	// Index.
	indexRecorder := indexCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if indexRecorder.Code != http.StatusOK {
		t.Fatalf("index status=%d body=%s", indexRecorder.Code, indexRecorder.Body.String())
	}

	// Lookup all.
	lookupRecorder := lookupIndexedCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String(), struct{}{})

	if lookupRecorder.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", lookupRecorder.Code, lookupRecorder.Body.String())
	}

	body := lookupRecorder.Body.String()

	// SECURITY: no source content crosses HTTP.
	if strings.Contains(body, "SECRET_VALUE_42") {
		t.Error("lookup response leaks source content")
	}

	// No absolute filesystem paths.
	if strings.Contains(body, dir) || strings.Contains(body, source) {
		t.Error("lookup response contains absolute path")
	}

	// No content field in response.
	if strings.Contains(body, `"content":`) {
		t.Error("lookup response carries a content field")
	}
}
