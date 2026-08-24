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

// filterCall posts /repositories/:id/filter.
func filterCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/filter", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestFilter_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := filterCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404 with no filesystem contact", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

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

		recorder := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready for corrupted dir", recorder.Code, recorder.Body.String())
		}
	})
}

func TestFilter_HappyPathDecisionsOnly(t *testing.T) { // 16. authenticated ownership + leakage checks
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"README.md":                  "readme marker",
		"go.mod":                     "module demo",
		"main.go":                    "SECRET_SOURCE_MARKER",
		".github/workflows/ci.yml":   "on: push",
		"assets/logo.png":            "\x89PNG",
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

	recorder := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID       string         `json:"repository_id"`
		TotalFiles         int            `json:"total_files"`
		IncludedFiles      int            `json:"included_files"`
		ExcludedFiles      int            `json:"excluded_files"`
		TotalSelectedBytes int64          `json:"total_selected_bytes"`
		Languages          map[string]int `json:"languages"`
		ExclusionSummary   map[string]int `json:"exclusion_summary"`
		Files              []struct {
			Path      string `json:"path"`
			Size      int64  `json:"size"`
			Extension string `json:"extension"`
			Category  string `json:"category"`
			Included  bool   `json:"included"`
			Reason    string `json:"reason"`
		} `json:"files"`
		SymlinksSkipped int    `json:"symlinks_skipped"`
		Status          string `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "filtered" {
		t.Errorf("identity/status wrong: %s / %s", payload.RepositoryID, payload.Status)
	}

	// Candidates: README.md, go.mod, main.go, ci.yml, logo.png, .env
	// (.git + node_modules pruned, escape.md symlink skipped).
	if payload.TotalFiles != 6 || payload.IncludedFiles != 4 || payload.ExcludedFiles != 2 {
		t.Errorf("counts wrong: total=%d included=%d excluded=%d (%+v)",
			payload.TotalFiles, payload.IncludedFiles, payload.ExcludedFiles, payload.Files)
	}

	if payload.SymlinksSkipped != 1 {
		t.Errorf("SymlinksSkipped = %d, want 1", payload.SymlinksSkipped)
	}

	reasons := make(map[string]string)

	for _, file := range payload.Files {
		reasons[file.Path] = file.Reason

		if strings.HasPrefix(file.Path, "/") || filepath.IsAbs(file.Path) {
			t.Errorf("absolute path exposed: %q", file.Path)
		}
	}

	wantReasons := map[string]string{
		"README.md":                "included_documentation",
		"go.mod":                   "included_config",
		"main.go":                  "included_source",
		".github/workflows/ci.yml": "included_config",
		"assets/logo.png":          "binary",
		".env":                     "secret",
	}

	for path, wantReason := range wantReasons {
		if reasons[path] != wantReason {
			t.Errorf("reason[%s] = %q, want %q", path, reasons[path], wantReason)
		}
	}

	if payload.Languages["Go"] != 1 || payload.Languages["YAML"] != 1 {
		t.Errorf("languages wrong: %v", payload.Languages)
	}

	if payload.TotalSelectedBytes == 0 {
		t.Errorf("TotalSelectedBytes = 0, want the sum of included sizes")
	}

	body := recorder.Body.String()

	// SECURITY: no absolute paths, no source content markers, no server
	// secrets, and no node_modules detail ever cross the boundary.
	for _, leaked := range []string{dir, source, outside, "SECRET_SOURCE_MARKER", "TOP SECRET", "left-pad", "SHOULD_NEVER_BE_INCLUDED"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q", leaked)
		}
	}

	// Filtering is read-only: ignored content stays on disk untouched.
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "left-pad", "x.js")); err != nil {
		t.Errorf("ignored directory content was deleted: %v", err)
	}

	if _, err := os.Stat(outsideSecret); err != nil {
		t.Errorf("outside file was touched: %v", err)
	}
}

func TestFilter_DeterministicResponses(t *testing.T) {
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	first := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	second := filterCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}

	if first.Body.String() != second.Body.String() {
		t.Errorf("responses differ between identical runs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}
