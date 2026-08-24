package repositories

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Aevor/platform/services/api/internal/workspace"
)

// extractCall posts /repositories/:id/extract.
func extractCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/extract", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestExtract_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := extractCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

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

		recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready for corrupted dir", recorder.Code, recorder.Body.String())
		}
	})
}

func TestExtract_HappyPathMetadataOnly(t *testing.T) { // 15. isolation + metadata-only boundary
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"README.md":                  "# Demo\n\nreadme marker\n",
		"go.mod":                     "module demo\n",
		"main.go":                    "SECRET_SOURCE_MARKER\n",
		".github/workflows/ci.yml":   "on: push\n",
		"assets/logo.png":            "\x89PNG\r\n\x1a\n",
		"node_modules/left-pad/x.js": "junk",
		".env":                       "SHOULD_NEVER_BE_INCLUDED=1",
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

	if err := os.Symlink(outsideSecret, filepath.Join(dir, "escape.md")); err != nil {
		t.Fatal(err)
	}

	recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID    string `json:"repository_id"`
		TotalCandidates int    `json:"total_candidates"`
		ExtractedCount  int    `json:"extracted_count"`
		ExtractedBytes  int64  `json:"extracted_bytes"`
		SkippedCount    int    `json:"skipped_count"`
		Complete        bool   `json:"complete"`
		Files           []struct {
			Path        string `json:"path"`
			Size        int64  `json:"size"`
			Extension   string `json:"extension"`
			Language    string `json:"language"`
			ContentHash string `json:"content_hash"`
		} `json:"files"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "extracted" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// Only what filtering included: README.md, go.mod, main.go, ci.yml.
	if payload.TotalCandidates != 4 || payload.ExtractedCount != 4 {
		t.Errorf("counts wrong: candidates=%d extracted=%d (%+v)",
			payload.TotalCandidates, payload.ExtractedCount, payload.Files)
	}

	if !payload.Complete || payload.SkippedCount != 0 {
		t.Errorf("complete/skipped = %v/%d, want true/0", payload.Complete, payload.SkippedCount)
	}

	wantPaths := map[string]bool{
		"README.md":                false,
		"go.mod":                   false,
		"main.go":                  false,
		".github/workflows/ci.yml": false,
	}

	for _, file := range payload.Files {
		if _, ok := wantPaths[file.Path]; !ok {
			t.Errorf("unexpected extracted file %q", file.Path)
		}
		wantPaths[file.Path] = true

		if len(file.ContentHash) != 64 {
			t.Errorf("file %q hash length = %d, want 64 hex chars", file.Path, len(file.ContentHash))
		}

		if file.Size <= 0 {
			t.Errorf("file %q size = %d", file.Path, file.Size)
		}
	}

	for path, seen := range wantPaths {
		if !seen {
			t.Errorf("expected file %q missing from extraction", path)
		}
	}

	body := recorder.Body.String()

	// SECURITY: contents NEVER cross HTTP — not even the key exists.
	if strings.Contains(body, `"content":`) {
		t.Error("response carries a content field")
	}

	for _, leaked := range []string{dir, source, outside, "SECRET_SOURCE_MARKER", "TOP SECRET", "readme marker", "module demo", "SHOULD_NEVER_BE_INCLUDED", "left-pad"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}

	// Read-only guarantees: ignored and outside content untouched.
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "left-pad", "x.js")); err != nil {
		t.Errorf("ignored directory content was deleted: %v", err)
	}

	if content, err := os.ReadFile(outsideSecret); err != nil || string(content) != "TOP SECRET SERVER FILE" {
		t.Errorf("outside file touched: %v %q", err, content)
	}
}

func TestExtract_DeterministicResponsesAndStableHashes(t *testing.T) {
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
		"src/app.go": "package app\n",
		"docs.md":    "documentation\n",
	}

	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(relative)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	second := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}

	if first.Body.String() != second.Body.String() {
		t.Errorf("responses differ between identical runs:\n%s\n%s", first.Body.String(), second.Body.String())
	}

	var payload struct {
		Files []struct {
			Path        string `json:"path"`
			ContentHash string `json:"content_hash"`
		} `json:"files"`
	}

	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Files) != 3 { // src/app.go + docs.md + the fixture's own README.md
		t.Fatalf("files = %d, want 3", len(payload.Files))
	}

	// Same bytes → same hash across independent runs (unchanged-file rule).
	for _, file := range payload.Files {
		if len(file.ContentHash) != 64 {
			t.Errorf("%q hash = %q", file.Path, file.ContentHash)
		}
	}
}

func TestExtract_NoSourceContentsInServerLogs(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	marker := "LOG_SECRET_SOURCE_MARKER_7f3a"

	if err := os.WriteFile(filepath.Join(dir, "secret.go"), []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "locked.go"), []byte("UNREADABLE_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(dir, "locked.go"), 0o000); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "locked.go"), 0o600) })
	}

	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)

	recorder := extractCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	log.SetOutput(previous)

	// The unreadable entry fails closed with a uniform internal error.
	if recorder.Code != http.StatusInternalServerError ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"internal"}` {
		t.Errorf("status=%d body=%s, want internal", recorder.Code, recorder.Body.String())
	}

	combined := logs.String() + recorder.Body.String()

	for _, forbidden := range []string{marker, "UNREADABLE_MARKER", dir, source} {
		if strings.Contains(combined, forbidden) {
			t.Errorf("logs/response leak %q", forbidden)
		}
	}
}
