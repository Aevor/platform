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

// discoverCall posts /repositories/:id/discover (fixture.call targets the
// clone route).
func discoverCall(t *testing.T, fixture *cloneFixture, jwtToken string, selectedID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/repositories/"+selectedID+"/discover", nil)
	recorder := httptest.NewRecorder()

	if jwtToken != "" {
		request.Header.Set("Authorization", "Bearer "+jwtToken)
	}

	fixture.router.ServeHTTP(recorder, request)

	return recorder
}

func TestDiscover_HandlerContract(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	t.Run("unauthenticated is uniform 401", func(t *testing.T) {
		recorder := discoverCall(t, fixture, "", cloneSelected.String())

		if recorder.Code != http.StatusUnauthorized || strings.TrimSpace(recorder.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("status=%d body=%s, want uniform unauthorized", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("malformed uuid is invalid_request", func(t *testing.T) {
		recorder := discoverCall(t, fixture, fixture.tokenFor(t, cloneUserID), "not-a-uuid")

		if recorder.Code != http.StatusBadRequest ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"invalid_request"}` {
			t.Errorf("status=%d body=%s, want invalid_request", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("foreign and unknown contexts are opaque 404 with no filesystem contact", func(t *testing.T) {
		for _, target := range []uuid.UUID{cloneSelectedFg, uuid.New()} {
			recorder := discoverCall(t, fixture, fixture.tokenFor(t, cloneUserID), target.String())

			if recorder.Code != http.StatusNotFound ||
				strings.TrimSpace(recorder.Body.String()) != `{"error":"repository_not_found"}` {
				t.Errorf("target %s: status=%d body=%s, want repository_not_found", target, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("missing workspace is workspace_not_ready", func(t *testing.T) {
		recorder := discoverCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

		if recorder.Code != http.StatusConflict ||
			strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
			t.Errorf("status=%d body=%s, want workspace_not_ready", recorder.Code, recorder.Body.String())
		}
	})
}

func TestDiscover_HappyPathMetadataOnly(t *testing.T) {
	outside := t.TempDir()
	source := initLocalGitRepo(t, filepath.Join(t.TempDir(), "source"))
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+source))
	fixture.service.cloner = workspace.NewGoGitCloner().WithDepth(0)

	if _, err := fixture.service.CloneRepository(context.Background(), cloneUserID, cloneSelected); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	dir := fixture.service.workspaces.Dir(cloneSelected)

	seeded := map[string]string{
		"README.md":                "demo",
		"go.mod":                   "module demo",
		"cmd/server/main.go":       "SECRET_SOURCE_MARKER",
		"web/app.tsx":              "export {}",
		".github/workflows/ci.yml": "on: push",
	}

	for relative, content := range seeded {
		full := filepath.Join(dir, relative)

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	junk := filepath.Join(dir, "node_modules/x/index.js")

	if err := os.MkdirAll(filepath.Dir(junk), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(junk, []byte("j"), 0o600); err != nil {
		t.Fatal(err)
	}

	outsideSecret := filepath.Join(outside, "secret.txt")

	if err := os.WriteFile(outsideSecret, []byte("TOP SECRET SERVER FILE"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outsideSecret, filepath.Join(dir, "escape.md")); err != nil {
		t.Fatal(err)
	}

	recorder := discoverCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RepositoryID   string         `json:"repository_id"`
		Files          int            `json:"files"`
		Directories    int            `json:"directories"`
		Languages      map[string]int `json:"languages"`
		ImportantFiles []string       `json:"important_files"`
		Status         string         `json:"status"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if payload.RepositoryID != cloneSelected.String() || payload.Status != "discovered" {
		t.Errorf("identity/status wrong: %+v", payload)
	}

	if payload.Files != 5 { // README, go.mod, main.go, app.tsx, ci.yml (.git + node_modules ignored, symlink skipped)
		t.Errorf("Files = %d (%+v)", payload.Files, payload)
	}

	if payload.Languages["Go"] != 1 || payload.Languages["TypeScript"] != 1 {
		t.Errorf("languages wrong: %v", payload.Languages)
	}

	foundReadme, foundWorkflow := false, false

	for _, important := range payload.ImportantFiles {
		if strings.Contains(important, "/") && !strings.HasPrefix(important, ".github/") {
			t.Errorf("important file path not workspace-relative-safe: %q", important)
		}

		if important == "README.md" {
			foundReadme = true
		}

		if important == ".github/workflows/ci.yml" {
			foundWorkflow = true
		}
	}

	if !foundReadme || !foundWorkflow {
		t.Errorf("expected markers missing from %v", payload.ImportantFiles)
	}

	body := recorder.Body.String()

	for _, leaked := range []string{dir, source, outside, "SECRET_SOURCE_MARKER", "TOP SECRET"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response leaks %q: %s", leaked, body)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "node_modules", "x", "index.js")); err != nil {
		t.Errorf("ignored directory content was deleted: %v", err)
	}

	if _, err := os.Stat(outsideSecret); err != nil {
		t.Errorf("outside file was touched: %v", err)
	}
}

func TestDiscover_CorruptedWorkspaceIsNotReady(t *testing.T) {
	fixture := newCloneFixture(t, githubRepoResponse(t, "file://"+filepath.Join(t.TempDir(), "unused")))

	junkDir, err := fixture.service.workspaces.Reset(cloneSelected)

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(junkDir, "leftover.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := discoverCall(t, fixture, fixture.tokenFor(t, cloneUserID), cloneSelected.String())

	if recorder.Code != http.StatusConflict ||
		strings.TrimSpace(recorder.Body.String()) != `{"error":"workspace_not_ready"}` {
		t.Errorf("status=%d body=%s, want workspace_not_ready for corrupted dir", recorder.Code, recorder.Body.String())
	}
}
