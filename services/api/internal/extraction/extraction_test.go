package extraction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aevor/platform/services/api/internal/filtering"
)

// newTestService pairs a filterer and an extractor over the SAME options so
// selection budgets and read caps stay coherent, exactly like production.
func newTestService(t *testing.T, mutate func(*filtering.Options)) *Service {
	t.Helper()

	options := filtering.Options{}

	if mutate != nil {
		mutate(&options)
	}

	filterer := filtering.NewService(options)

	return NewService(filterer, Options{
		MaxFileSize: options.MaxFileSize,
	})
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for relativePath, content := range files {
		full := filepath.Join(root, filepath.FromSlash(relativePath))

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", relativePath, err)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", relativePath, err)
		}
	}

	return root
}

func findFile(t *testing.T, result *Result, path string) *File {
	t.Helper()

	for i := range result.Files {
		if result.Files[i].Path == path {
			return &result.Files[i]
		}
	}

	t.Fatalf("file %q not extracted", path)
	return nil
}

func hasPath(result *Result, path string) bool {
	for i := range result.Files {
		if result.Files[i].Path == path {
			return true
		}
	}

	return false
}

const knownHelloHash = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
const knownEmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestExtract_NormalUTF8SourceFile(t *testing.T) {
	source := "package main\n\nfunc main() {}\n"
	root := writeTree(t, map[string]string{"main.go": source})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	file := findFile(t, result, "main.go")

	if file.Content != source {
		t.Errorf("content not preserved exactly")
	}
	if file.Language != "Go" || file.Extension != ".go" {
		t.Errorf("language/extension = %q/%q, want Go/.go", file.Language, file.Extension)
	}
	if file.Size != int64(len(source)) {
		t.Errorf("size = %d, want %d", file.Size, len(source))
	}
	if !result.Complete || result.ExtractedCount != 1 {
		t.Errorf("complete/count = %v/%d", result.Complete, result.ExtractedCount)
	}
}

func TestExtract_MultilineAndCRLFPreserved(t *testing.T) {
	source := "line one\r\nline two\r\n\r\nline four\n"
	root := writeTree(t, map[string]string{"windows.md": source})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	file := findFile(t, result, "windows.md")

	if file.Content != source {
		t.Errorf("CRLF content altered:\n%q", file.Content)
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	root := writeTree(t, map[string]string{"empty.go": ""})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	file := findFile(t, result, "empty.go")

	if file.Content != "" || file.Size != 0 {
		t.Errorf("empty file content/size = %q/%d", file.Content, file.Size)
	}
	if file.ContentHash != knownEmptyHash {
		t.Errorf("empty hash = %q, want %q", file.ContentHash, knownEmptyHash)
	}
}

func TestExtract_InvalidUTF8Skipped(t *testing.T) {
	root := t.TempDir()
	broken := append([]byte("readable prefix "), 0xff, 0xfe, 0x80)

	if err := os.WriteFile(filepath.Join(root, "notes.md"), broken, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if hasPath(result, "notes.md") {
		t.Error("invalid UTF-8 file was extracted")
	}
	if result.Skipped[ReasonInvalidUTF8] != 1 {
		t.Errorf("skipped = %v, want invalid_utf8:1", result.Skipped)
	}
	if result.Complete {
		t.Error("result should not be complete after skip")
	}
}

func TestExtract_BinaryContentNeverExtracted(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xd8, 0x00}
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "logo.png"), pngBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if hasPath(result, "logo.png") {
		t.Error("binary-classified file was extracted")
	}
	if result.TotalCandidates != 0 {
		t.Errorf("binary file reached candidates: %d", result.TotalCandidates)
	}
}

func TestExtract_OversizedFilteredBeforeExtraction(t *testing.T) {
	big := strings.Repeat("a", 40*1024)
	root := writeTree(t, map[string]string{
		"big.go": big,
		"ok.md":  "small",
	})

	extractor := newTestService(t, func(o *filtering.Options) { o.MaxFileSize = 1024 })

	result, err := extractor.Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if hasPath(result, "big.go") {
		t.Error("oversized file was extracted")
	}
	if !hasPath(result, "ok.md") {
		t.Error("small file missing")
	}
	if result.ExtractedBytes >= 40*1024 {
		t.Errorf("extracted bytes = %d, oversized file leaked in", result.ExtractedBytes)
	}
}

func TestExtract_ReadTimeGrowthDetected(t *testing.T) {
	// The filterer allows the file (well under its own cap) while the
	// extractor's read-time cap is smaller — proving per-file size is
	// re-verified at open time, closing the filter-to-read race.
	grown := strings.Repeat("b", 20)
	root := writeTree(t, map[string]string{"grown.go": grown})

	filterer := filtering.NewService(filtering.Options{})
	extractor := NewService(filterer, Options{MaxFileSize: 10})

	result, err := extractor.Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if hasPath(result, "grown.go") {
		t.Error("over-cap file was extracted at read time")
	}
	if result.Skipped[ReasonTooLarge] != 1 {
		t.Errorf("skipped = %v, want too_large:1", result.Skipped)
	}
}

func TestExtract_NestedFileRelativePathsOnly(t *testing.T) {
	root := writeTree(t, map[string]string{"a/b/c/deep.py": "print('hi')\n"})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	file := findFile(t, result, "a/b/c/deep.py")

	if strings.Contains(file.Path, string(os.PathSeparator)) && !strings.Contains(file.Path, "/") {
		t.Errorf("path not slash-normalized: %q", file.Path)
	}
	if file.Content != "print('hi')\n" {
		t.Errorf("nested content = %q", file.Content)
	}
}

func TestExtractDecisions_RejectsTraversalAndAbsolutePaths(t *testing.T) {
	root := t.TempDir()

	outside := t.TempDir()
	outsideSecret := "OUTSIDE_SECRET_CONTENT"
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte(outsideSecret), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"parent traversal", "../outside.txt"},
		{"nested traversal", "sub/../../outside.txt"},
		{"absolute path", "/etc/passwd"},
		{"null byte", "main\x00.go"},
		{"backtrack to root", "../../" + filepath.Base(outside) + "/outside.txt"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := newTestService(t, nil).ExtractDecisions(
				context.Background(),
				root,
				[]filtering.FileDecision{{Path: testCase.path, Included: true}},
			)

			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("err = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func TestExtract_SymlinkEscapeRejected(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.go": "package keep\n"})

	outside := t.TempDir()
	outsideSecret := "TOPSECRET_OUTSIDE"
	secretPath := filepath.Join(outside, "secrets.env")
	if err := os.WriteFile(secretPath, []byte(outsideSecret), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	if err := os.Symlink(secretPath, filepath.Join(root, "link.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Simulate a post-filter swap: the decision claims link.go is included,
	// but the leaf is now a symlink pointing outside the workspace.
	_, err := newTestService(t, nil).ExtractDecisions(
		context.Background(),
		root,
		[]filtering.FileDecision{{
			Path:     "link.go",
			Size:     14,
			Language: "Go",
			Included: true,
			Reason:   filtering.ReasonIncludedSource,
		}},
	)

	if err != nil {
		t.Fatalf("extract decisions: %v", err)
	}

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	for i := range result.Files {
		if strings.Contains(result.Files[i].Content, outsideSecret) {
			t.Fatal("out-of-workspace secret leaked into extraction")
		}
	}
	if hasPath(result, "link.go") {
		t.Error("symlink was extracted")
	}
}

func TestExtract_SymlinkedDirectoryEscapeFailsClosed(t *testing.T) {
	root := writeTree(t, map[string]string{})

	outside := t.TempDir()
	outsideSecret := "TOPSECRET_DIR_ESCAPE"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideSecret), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := newTestService(t, nil).ExtractDecisions(
		context.Background(),
		root,
		[]filtering.FileDecision{{Path: "escape/secret.txt", Included: true}},
	)

	if !errors.Is(err, ErrUnreadableEntry) {
		t.Fatalf("err = %v, want ErrUnreadableEntry (fail closed)", err)
	}
}

func TestExtract_DeterministicOrdering(t *testing.T) {
	root := writeTree(t, map[string]string{
		"z.go":          "package z\n",
		"a.md":          "# A\n",
		"m/n/nested.py": "x = 1\n",
		"b.txt":         "bee\n",
	})

	service := newTestService(t, nil)

	want := []string{"a.md", "b.txt", "m/n/nested.py", "z.go"}

	for run := 0; run < 3; run++ {
		result, err := service.Extract(context.Background(), root)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}

		if len(result.Files) != len(want) {
			t.Fatalf("got %d files, want %d", len(result.Files), len(want))
		}

		for i, file := range result.Files {
			if file.Path != want[i] {
				t.Errorf("position %d = %q, want %q", i, file.Path, want[i])
			}
		}
	}
}

func TestExtract_KnownSHA256Vector(t *testing.T) {
	root := writeTree(t, map[string]string{"hello.txt": "hello"})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	file := findFile(t, result, "hello.txt")

	if file.ContentHash != knownHelloHash {
		t.Errorf("hash = %q, want %q", file.ContentHash, knownHelloHash)
	}

	direct := sha256.Sum256([]byte("hello"))

	if file.ContentHash != hex.EncodeToString(direct[:]) {
		t.Error("hash is not SHA-256 of exact bytes")
	}
}

func TestExtract_HashStableUnchangedDifferentChanged(t *testing.T) {
	root := writeTree(t, map[string]string{"app.js": "console.log(1);\n"})

	first, err := newTestService(t, nil).Extract(context.Background(), root)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	unchanged, err := newTestService(t, nil).Extract(context.Background(), root)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.Files[0].ContentHash != unchanged.Files[0].ContentHash {
		t.Error("unchanged content produced different hash")
	}

	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log(42);\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	changed, err := newTestService(t, nil).Extract(context.Background(), root)
	if err != nil {
		t.Fatalf("third: %v", err)
	}

	if changed.Files[0].ContentHash == first.Files[0].ContentHash {
		t.Error("changed content produced identical hash")
	}
}

func TestExtract_FilteringIntegration(t *testing.T) {
	envSecret := "API_KEY=super-secret-value-do-not-log"
	root := writeTree(t, map[string]string{
		".env":                      envSecret,
		"node_modules/lib/index.js": "module.exports = () => 'vendored';\n",
		"README.md":                 "# Project\n",
		"src/main.go":               "package main\n",
		"go.mod":                    "module example.com/demo\n\ngo 1.26\n",
		"data.log":                  "2026-08-24 noisy log line\n",
	})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	for _, excluded := range []string{".env", "node_modules/lib/index.js", "data.log"} {
		if hasPath(result, excluded) {
			t.Errorf("%q was extracted", excluded)
		}
	}

	for _, included := range []string{"README.md", "src/main.go", "go.mod"} {
		findFile(t, result, included)
	}

	if result.TotalCandidates != 3 {
		t.Errorf("candidates = %d, want 3", result.TotalCandidates)
	}
}

func TestExtract_TotalSizeBudgetRespected(t *testing.T) {
	sixtyA := strings.Repeat("a", 60)
	sixtyB := strings.Repeat("b", 60)
	thirty := strings.Repeat("c", 30)

	root := writeTree(t, map[string]string{
		"a_first.go":  sixtyA,
		"b_second.go": sixtyB,
		"c_third.md":  thirty,
	})

	extractor := newTestService(t, func(o *filtering.Options) { o.MaxTotalBytes = 100 })

	result, err := extractor.Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if result.ExtractedBytes > 100 {
		t.Errorf("extracted %d bytes, budget is 100", result.ExtractedBytes)
	}
	if !hasPath(result, "a_first.go") || !hasPath(result, "c_third.md") {
		t.Errorf("expected budget-fitting files, got %+v", result.Skipped)
	}
	if hasPath(result, "b_second.go") {
		t.Error("budget-overflowing file was extracted")
	}
}

func TestExtract_MaxFileCountRespected(t *testing.T) {
	root := writeTree(t, map[string]string{
		"one.go":   "one\n",
		"two.go":   "two\n",
		"three.go": "three\n",
		"four.go":  "four\n",
	})

	extractor := newTestService(t, func(o *filtering.Options) { o.MaxSelectedFiles = 2 })

	result, err := extractor.Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if result.ExtractedCount != 2 {
		t.Errorf("extracted %d files, want 2", result.ExtractedCount)
	}

	if len(result.Files) != 2 {
		t.Fatalf("files list = %d entries, want 2", len(result.Files))
	}
}

func TestExtract_NoContentsInErrorsOrResults(t *testing.T) {
	envSecret := "DATABASE_URL=postgres://user:hunter2@localhost/db"
	root := writeTree(t, map[string]string{
		".env":        envSecret,
		"binary.blob": "\xff\xfe\x00\x01",
	})

	result, err := newTestService(t, nil).Extract(context.Background(), root)

	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	fingerprints := []string{envSecret, "hunter2", "\xff\xfe\x00\x01"}

	var extracted strings.Builder

	for _, file := range result.Files {
		extracted.WriteString(file.Path)
		extracted.WriteString(file.Content)
	}

	check := func(where string, value string) {
		t.Helper()

		for _, fingerprint := range fingerprints {
			if strings.Contains(value, fingerprint) {
				t.Errorf("%s leaks sensitive content (%q present)", where, fingerprint)
			}
		}
	}

	check("result files", extracted.String())

	// Error paths must never carry contents either.
	if _, err := newTestService(t, nil).ExtractDecisions(
		context.Background(),
		root,
		[]filtering.FileDecision{{Path: "no/such/file.go", Included: true}},
	); err != nil {
		check("error string", err.Error())
	}
}

func TestExtract_EmptyWorkspace(t *testing.T) {
	result, err := newTestService(t, nil).Extract(context.Background(), t.TempDir())

	if err != nil {
		t.Fatalf("extract empty: %v", err)
	}

	if result.ExtractedCount != 0 || result.TotalCandidates != 0 || !result.Complete {
		t.Errorf("unexpected result for empty workspace: %+v", result)
	}
}

func TestExtract_CancelledContext(t *testing.T) {
	root := writeTree(t, map[string]string{"main.go": "package main\n"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Cancellation during the filtering phase surfaces the filterer's own
	// deadline sentinel; handlers map both to the same external code.
	if _, err := newTestService(t, nil).Extract(ctx, root); !errors.Is(err, filtering.ErrTimeout) {
		t.Fatalf("extract err = %v, want filtering.ErrTimeout", err)
	}

	// Cancellation during the read phase surfaces extraction's sentinel.
	decisions := []filtering.FileDecision{{Path: "main.go", Included: true}}

	if _, err := newTestService(t, nil).ExtractDecisions(ctx, root, decisions); !errors.Is(err, ErrTimeout) {
		t.Fatalf("decisions err = %v, want ErrTimeout", err)
	}
}

func TestExtract_UnreadableEntryFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}

	root := writeTree(t, map[string]string{
		"locked.go": "package locked\n",
		"open.md":   "readable\n",
	})

	if err := os.Chmod(filepath.Join(root, "locked.go"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked.go"), 0o600) })

	_, err := newTestService(t, nil).Extract(context.Background(), root)

	if !errors.Is(err, ErrUnreadableEntry) {
		t.Fatalf("err = %v, want ErrUnreadableEntry", err)
	}
}

func TestExtractDecisions_NotSelectedAndDuplicatesSkipped(t *testing.T) {
	root := writeTree(t, map[string]string{"main.go": "package main\n"})

	result, err := newTestService(t, nil).ExtractDecisions(
		context.Background(),
		root,
		[]filtering.FileDecision{
			{Path: "main.go", Included: false, Reason: filtering.ReasonIgnoredDirectory},
			{Path: "main.go", Included: true},
			{Path: "main.go", Included: true},
		},
	)

	if err != nil {
		t.Fatalf("extract decisions: %v", err)
	}

	if result.ExtractedCount != 1 {
		t.Errorf("extracted = %d, want 1 (first occurrence wins)", result.ExtractedCount)
	}
	if result.Skipped[ReasonNotSelected] != 1 ||
		result.Skipped[ReasonDuplicatePath] != 1 {
		t.Errorf("skipped = %v, want not_selected:1 duplicate_path:1", result.Skipped)
	}
}
