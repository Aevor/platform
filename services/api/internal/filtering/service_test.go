package filtering

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedFile(t *testing.T, root string, relative string, content []byte) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(relative))

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}

	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func decisionFor(t *testing.T, result *Result, relative string) FileDecision {
	t.Helper()

	for _, file := range result.Files {
		if file.Path == relative {
			return file
		}
	}

	t.Fatalf("no decision recorded for %q in %+v", relative, result.Files)

	return FileDecision{}
}

func TestFilter_SelectionBasics(t *testing.T) {
	root := t.TempDir()

	seedFile(t, root, "main.go", []byte("package main"))                    // 1. source included
	seedFile(t, root, "cmd/server/main.go", []byte("package main"))         // 10. nested source
	seedFile(t, root, "README.md", []byte("# demo"))                        // 2. documentation
	seedFile(t, root, "LICENSE", []byte("MIT"))                             // documentation by name
	seedFile(t, root, "config/settings.yaml", []byte("key: value"))         // 3. configuration
	seedFile(t, root, ".github/workflows/ci.yml", []byte("on: push"))       // configuration
	seedFile(t, root, "assets/logo.png", []byte{0x89, 'P', 'N', 'G', 0x00}) // 7. binary excluded
	seedFile(t, root, "plugin.unknownext", []byte("x"))                     // 9. unsupported
	seedFile(t, root, "debug.log", []byte("noise"))                         // junk text

	service := NewService(Options{})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if got := decisionFor(t, result, "main.go"); !got.Included || got.Category != CategorySource || got.Reason != ReasonIncludedSource || got.Language != "Go" {
		t.Errorf("main.go = %+v", got)
	}

	if got := decisionFor(t, result, "cmd/server/main.go"); !got.Included || got.Path != "cmd/server/main.go" {
		t.Errorf("nested main.go = %+v", got)
	}

	if got := decisionFor(t, result, "README.md"); !got.Included || got.Reason != ReasonIncludedDocs {
		t.Errorf("README.md = %+v", got)
	}

	if got := decisionFor(t, result, "LICENSE"); !got.Included || got.Reason != ReasonIncludedDocs {
		t.Errorf("LICENSE = %+v", got)
	}

	if got := decisionFor(t, result, "config/settings.yaml"); !got.Included || got.Reason != ReasonIncludedConfig {
		t.Errorf("settings.yaml = %+v", got)
	}

	if got := decisionFor(t, result, ".github/workflows/ci.yml"); !got.Included || got.Reason != ReasonIncludedConfig {
		t.Errorf("ci.yml = %+v", got)
	}

	if got := decisionFor(t, result, "assets/logo.png"); got.Included || got.Reason != ReasonBinary {
		t.Errorf("logo.png = %+v", got)
	}

	if got := decisionFor(t, result, "plugin.unknownext"); got.Included || got.Reason != ReasonUnsupported {
		t.Errorf("unknown extension = %+v", got)
	}

	if got := decisionFor(t, result, "debug.log"); got.Included || got.Reason != ReasonIgnoredExtension {
		t.Errorf("debug.log = %+v", got)
	}

	if result.IncludedFiles != 6 {
		t.Errorf("IncludedFiles = %d, want 6 (%+v)", result.IncludedFiles, result.Files)
	}

	if result.TotalSelectedBytes == 0 {
		t.Errorf("TotalSelectedBytes = 0, want the sum of included sizes")
	}

	var sum int64

	for _, file := range result.Files {
		if file.Included {
			sum += file.Size
		}
	}

	if sum != result.TotalSelectedBytes {
		t.Errorf("TotalSelectedBytes = %d, but included decisions sum to %d", result.TotalSelectedBytes, sum)
	}

	if result.Languages["Go"] != 2 || result.Languages["YAML"] != 2 {
		t.Errorf("languages wrong: %v", result.Languages)
	}
}

func TestFilter_IgnoredDirectoriesPrunedButPreservedOnDisk(t *testing.T) {
	root := t.TempDir() // 4./5./6. node_modules, .git, build/dist excluded

	for _, ignored := range []string{
		".git/HEAD",
		"node_modules/left-pad/index.js",
		"build/output.o",
		"dist/bundle.js",
		"vendor/example/pkg/pkg.go",
		"target/release/app",
		"__pycache__/app.cpython.pyc",
		".venv/lib/pyvenv.cfg",
		"coverage/lcov.info",
	} {
		seedFile(t, root, ignored, []byte("junk"))
	}

	seedFile(t, root, "main.go", []byte("package main"))

	service := NewService(Options{})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if result.TotalFiles != 1 || result.IncludedFiles != 1 {
		t.Errorf("counts = %d/%d, want 1/1 (only main.go outside pruned dirs)", result.TotalFiles, result.IncludedFiles)
	}

	if result.IgnoredDirectories < 8 {
		t.Errorf("IgnoredDirectories = %d, want >= 8", result.IgnoredDirectories)
	}

	if got := result.ExclusionSummary[ReasonIgnoredDirectory]; got != result.IgnoredDirectories {
		t.Errorf("exclusion_summary[ignored_directory] = %d, want %d", got, result.IgnoredDirectories)
	}

	// Ignored content is NEVER deleted — exclusion is logical only.
	for _, kept := range []string{
		"node_modules/left-pad/index.js",
		".git/HEAD",
		"vendor/example/pkg/pkg.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(kept))); err != nil {
			t.Errorf("ignored content was deleted: %s: %v", kept, err)
		}
	}
}

func TestFilter_OversizedFileExcludedWithReason(t *testing.T) { // 8.
	root := t.TempDir()

	seedFile(t, root, "small.go", []byte("package main"))
	seedFile(t, root, "big.generated", make([]byte, 4096))

	service := NewService(Options{MaxFileSize: 1024})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	got := decisionFor(t, result, "big.generated")

	if got.Included || got.Reason != ReasonTooLarge {
		t.Errorf("oversized file = %+v, want excluded too_large", got)
	}

	if result.ExclusionSummary[ReasonTooLarge] != 1 {
		t.Errorf("exclusion_summary[too_large] = %d, want 1", result.ExclusionSummary[ReasonTooLarge])
	}

	if result.TotalSelectedBytes != int64(len("package main")) {
		t.Errorf("TotalSelectedBytes = %d, want only the small file's bytes", result.TotalSelectedBytes)
	}
}

func TestFilter_SymlinkEscapeNeverFollowed(t *testing.T) { // 12.
	outside := t.TempDir()

	secret := filepath.Join(outside, "server-secret.txt")

	if err := os.WriteFile(secret, []byte("TOP SECRET SERVER FILE"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()

	seedFile(t, root, "main.go", []byte("package main"))

	if err := os.Symlink(secret, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatal(err)
	}

	service := NewService(Options{})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if result.TotalFiles != 1 || result.SymlinksSkipped != 2 {
		t.Errorf("TotalFiles=%d SymlinksSkipped=%d, want 1/2", result.TotalFiles, result.SymlinksSkipped)
	}

	for _, file := range result.Files {
		if file.Path == "escape.md" || file.Path == "escape-dir" {
			t.Errorf("symlink reached decisions: %+v", file)
		}
	}
}

func TestFilter_GitignoreDoesNotInfluencePolicy(t *testing.T) { // 13. documented decision
	root := t.TempDir()

	// A .gitignore that "should" hide both entries must NOT change outcomes:
	// Git ignore rules are build hygiene, not Aevor analysis policy.
	seedFile(t, root, ".gitignore", []byte("generated.txt\ndocs/\n"))
	seedFile(t, root, "generated.txt", []byte("data"))
	seedFile(t, root, "docs/guide.md", []byte("guide"))
	seedFile(t, root, "main.go", []byte("package main"))

	service := NewService(Options{})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if got := decisionFor(t, result, "generated.txt"); !got.Included {
		t.Errorf(".gitignore'd-but-includable file was excluded: %+v (policy must stay independent of .gitignore)", got)
	}

	if got := decisionFor(t, result, ".gitignore"); !got.Included || got.Reason != ReasonIncludedConfig {
		t.Errorf(".gitignore itself = %+v, want included as config", got)
	}
}

func TestFilter_MaxSelectedFilesTruncates(t *testing.T) { // 14.
	root := t.TempDir()

	const seeded = 25

	for i := 0; i < seeded; i++ {
		seedFile(t, root, "pkg"+string(rune('a'+i))+"/file"+string(rune('a'+i))+".go", []byte("package main"))
	}

	service := NewService(Options{MaxSelectedFiles: 10})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if result.IncludedFiles != 10 {
		t.Errorf("IncludedFiles = %d, want exactly the limit", result.IncludedFiles)
	}

	if !result.Truncated {
		t.Errorf("Truncated = false, want true when the selection budget is hit")
	}

	limited := 0

	for _, file := range result.Files {
		if file.Reason == ReasonFileCountLimit {
			limited++
		}
	}

	if limited == 0 {
		t.Errorf("no candidate carries reason %q", ReasonFileCountLimit)
	}

	if result.ExclusionSummary[ReasonFileCountLimit] != seeded-10 {
		t.Errorf("exclusion_summary[file_count_limit] = %d, want %d", result.ExclusionSummary[ReasonFileCountLimit], seeded-10)
	}
}

func TestFilter_TotalSizeBudgetExhausted(t *testing.T) { // 15.
	root := t.TempDir()

	seedFile(t, root, "a_first.go", []byte("package a"))
	seedFile(t, root, "b_second.go", make([]byte, 2048))
	seedFile(t, root, "c_third.go", []byte("package c"))

	service := NewService(Options{MaxTotalBytes: 100})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	if result.IncludedFiles != 2 {
		t.Errorf("IncludedFiles = %d, want 2 (first file + the later small one that still fits)", result.IncludedFiles)
	}

	if !result.Truncated {
		t.Errorf("Truncated = false, want true when the size budget is exhausted")
	}

	if result.TotalSelectedBytes > 100 {
		t.Errorf("TotalSelectedBytes = %d, exceeded the 100-byte budget", result.TotalSelectedBytes)
	}

	budgetHits := 0

	for _, file := range result.Files {
		switch file.Path {
		case "a_first.go":
			if !file.Included {
				t.Errorf("first file must fit the budget: %+v", file)
			}
		case "b_second.go":
			if file.Included || file.Reason != ReasonTotalSizeLimit {
				t.Errorf("%s = %+v, want excluded total_size_limit", file.Path, file)
			}

			budgetHits++
		case "c_third.go":
			// Documented semantics: the budget caps the RUNNING TOTAL; a
			// later candidate small enough to still fit stays included.
			if !file.Included {
				t.Errorf("%s = %+v, want included (still within budget)", file.Path, file)
			}
		}
	}

	if budgetHits != 1 {
		t.Errorf("budget-limit hits = %d, want 1", budgetHits)
	}
}

func TestFilter_DeterministicOutput(t *testing.T) { // 18.
	root := t.TempDir()

	seedFile(t, root, "z_last.md", []byte("docs"))
	seedFile(t, root, "src/main.go", []byte("package main"))
	seedFile(t, root, "README.md", []byte("readme"))
	seedFile(t, root, "node_modules/x/index.js", []byte("junk"))
	seedFile(t, root, "asset.png", []byte{0x00, 0x01})

	service := NewService(Options{})

	first, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("first Filter(): %v", err)
	}

	second, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("second Filter(): %v", err)
	}

	firstJSON, err := json.Marshal(first)

	if err != nil {
		t.Fatal(err)
	}

	secondJSON, err := json.Marshal(second)

	if err != nil {
		t.Fatal(err)
	}

	if string(firstJSON) != string(secondJSON) {
		t.Errorf("filter output is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}

	for i := 1; i < len(first.Files); i++ {
		if first.Files[i-1].Path >= first.Files[i].Path {
			t.Errorf("decisions not sorted at %d: %q >= %q", i, first.Files[i-1].Path, first.Files[i].Path)
		}
	}
}

func TestFilter_RelativePathsOnlyAndNoContents(t *testing.T) { // 11. containment guarantees
	root := t.TempDir()

	seedFile(t, root, "src/deep/nested/module/file.go", []byte("MARKER_SOURCE_CONTENT"))

	service := NewService(Options{})

	result, err := service.Filter(context.Background(), root)

	if err != nil {
		t.Fatalf("Filter() error: %v", err)
	}

	serialized, err := json.Marshal(result)

	if err != nil {
		t.Fatal(err)
	}

	body := string(serialized)

	if strings.Contains(body, "MARKER_SOURCE_CONTENT") {
		t.Error("file content leaked into the result")
	}

	if len(result.Files) == 0 {
		t.Fatal("expected decisions")
	}

	if os.PathSeparator != '/' {
		// On non-slash platforms the JSON contract must still be slash-only.
		for _, file := range result.Files {
			for _, r := range file.Path {
				if r == os.PathSeparator {
					t.Errorf("native separator leaked into path %q", file.Path)
				}
			}
		}
	}
}

func TestFilter_EmptyAndMissingRoots(t *testing.T) {
	service := NewService(Options{})

	t.Run("empty workspace yields zeroed result", func(t *testing.T) {
		result, err := service.Filter(context.Background(), t.TempDir())

		if err != nil {
			t.Fatalf("Filter() error: %v", err)
		}

		if result.TotalFiles != 0 || result.IncludedFiles != 0 || result.TotalSelectedBytes != 0 {
			t.Errorf("empty workspace produced %+v", result)
		}

		if result.Status != "filtered" || result.Truncated || result.FilesTruncated {
			t.Errorf("flags wrong: %+v", result)
		}
	})

	t.Run("missing root is an unreadable-entry error", func(t *testing.T) {
		if _, err := service.Filter(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("expected an error for a missing root")
		}
	})
}

func TestFilter_CancelledContext(t *testing.T) {
	root := t.TempDir()

	seedFile(t, root, "main.go", []byte("package main"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	service := NewService(Options{})

	if _, err := service.Filter(ctx, root); err == nil {
		t.Fatal("expected ErrTimeout on a cancelled context")
	}
}
