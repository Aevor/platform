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

// representCall posts /repositories/:id/represent.
func representCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/represent", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestRepresent_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := representCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})
}

func TestRepresent_HappyPathMetadataOnly(t *testing.T) {
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
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
		".env":                     "SHOULD_NEVER_BE_REPRESENTED=1",
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

	recorder := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID string         `json:"repository_id"`
		TotalFiles   int            `json:"total_files"`
		TotalChunks  int            `json:"total_chunks"`
		RoleCounts   map[string]int `json:"role_counts"`
		Entries      []struct {
			ID              string  `json:"id"`
			FilePath        string  `json:"file_path"`
			Directory       string  `json:"directory"`
			FileRole        string  `json:"file_role"`
			Language        string  `json:"language"`
			ChunkIndex      int     `json:"chunk_index"`
			StartLine       int     `json:"start_line"`
			EndLine         int     `json:"end_line"`
			ContentHash     string  `json:"content_hash"`
			SymbolName      *string `json:"symbol_name"`
			SymbolType      string  `json:"symbol_type"`
			PrevChunkIndex  *int    `json:"prev_chunk_index"`
			NextChunkIndex  *int    `json:"next_chunk_index"`
			SourceUnderTest string  `json:"source_under_test"`
		} `json:"representations"`
		TruncatedFlag bool   `json:"representations_truncated"`
		Status        string `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Only filtering-selected files are represented; the symlink, .env and
	// binary junk never enter.
	if payload.RepositoryID != cloneSelected.String() || payload.Status != "represented" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	if payload.TotalFiles != 5 || payload.TotalChunks == 0 || len(payload.Entries) != payload.TotalChunks {
		t.Errorf("counts wrong: files=%d chunks=%d entries=%d", payload.TotalFiles, payload.TotalChunks, len(payload.Entries))
	}

	byPath := map[string]map[string]bool{}

	for _, entry := range payload.Entries {
		if byPath[entry.FilePath] == nil {
			byPath[entry.FilePath] = map[string]bool{}
		}
		byPath[entry.FilePath][entry.FileRole] = true

		// Deterministic identity shape: 64 lowercase hex chars.
		if len(entry.ID) != 64 {
			t.Errorf("%s chunk %d: id = %q, want 64 hex chars", entry.FilePath, entry.ChunkIndex, entry.ID)
		}
	}

	wantRoles := map[string]string{
		"main.go":                  "source",
		"main_test.go":             "test",
		"README.md":                "documentation",
		"go.mod":                   "dependency_manifest",
		".github/workflows/ci.yml": "ci",
	}

	for path, role := range wantRoles {
		if !byPath[path][role] {
			t.Errorf("%s roles = %v, want %q present", path, byPath[path], role)
		}
	}

	if payload.RoleCounts["test"] == 0 || payload.RoleCounts["ci"] == 0 {
		t.Errorf("role counts wrong: %+v", payload.RoleCounts)
	}

	// Traceability + conservative symbols: a Go function chunk carries symbol
	// metadata; test file links to its source counterpart.
	sawSymbol, sawTestLink := false, false

	for _, entry := range payload.Entries {
		if entry.SymbolName != nil && entry.SymbolType == "function" {
			sawSymbol = true
		}

		if entry.FilePath == "main_test.go" && entry.SourceUnderTest == "main.go" {
			sawTestLink = true
		}

		if entry.StartLine < 1 || entry.EndLine < entry.StartLine {
			t.Errorf("%s: invalid line span [%d,%d]", entry.FilePath, entry.StartLine, entry.EndLine)
		}
	}

	if !sawSymbol || !sawTestLink {
		t.Errorf("traceability wrong: symbol=%v testLink=%v", sawSymbol, sawTestLink)
	}

	body := recorder.Body.String()

	// SECURITY: contents NEVER cross HTTP — not even a content key exists;
	// no markers, secrets, or absolute paths leak.
	if strings.Contains(body, `"content"`) {
		t.Error("response carries a content field")
	}

	for _, leaked := range []string{dir, source, outside, "SOURCE_MARKER", "TOP SECRET", "documentation marker", "SHOULD_NEVER_BE_REPRESENTED", ".env", "escape"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}
}

func TestRepresent_DeterministicResponses(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
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

	first := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	second := representCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, first.Body.String())
	}

	if first.Body.String() != second.Body.String() {
		t.Errorf("responses differ between identical runs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}
