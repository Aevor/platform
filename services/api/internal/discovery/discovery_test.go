package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscover_NestedTreeCountsLanguagesAndImportantFiles(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "README.md"), "demo")
	writeFile(t, filepath.Join(root, "go.mod"), "module demo")
	writeFile(t, filepath.Join(root, "cmd/server/main.go"), "package main")
	writeFile(t, filepath.Join(root, "internal/web/app.tsx"), "export {}")
	writeFile(t, filepath.Join(root, "scripts/deploy.sh"), "#!/bin/sh")
	writeFile(t, filepath.Join(root, "docs/guide.md"), "text")
	writeFile(t, filepath.Join(root, "assets/logo.png"), "not-a-real-png")
	writeFile(t, filepath.Join(root, ".github/workflows/ci.yml"), "on: push")

	service := NewService(Options{})

	summary, err := service.Discover(context.Background(), root)

	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if summary.Files != 8 {
		t.Errorf("Files = %d, want 8 (every regular file counted once)", summary.Files)
	}

	if summary.Directories != 9 {
		t.Errorf("Directories = %d, want 9 (cmd,server,internal,web,scripts,docs,assets,.github,workflows)", summary.Directories)
	}

	if got := summary.Languages["Go"]; got != 1 {
		t.Errorf("Languages[Go] = %d, want 1", got)
	}

	if got := summary.Languages["TypeScript"]; got != 1 {
		t.Errorf("Languages[TypeScript] = %d, want 1 (.tsx)", got)
	}

	if got := summary.Languages["Shell"]; got != 1 {
		t.Errorf("Languages[Shell] = %d, want 1", got)
	}

	if _, binary := summary.Languages["PNG"]; binary {
		t.Errorf("binary asset leaked into languages: %v", summary.Languages)
	}

	wantImportant := map[string]bool{
		"README.md":                true,
		"go.mod":                   true,
		".github/workflows/ci.yml": true,
	}

	for _, important := range summary.ImportantFiles {
		if !wantImportant[important] {
			t.Errorf("unexpected important file %q (all: %v)", important, summary.ImportantFiles)
		}

		delete(wantImportant, important)
	}

	for missing := range wantImportant {
		t.Errorf("important file %q missing from %v", missing, summary.ImportantFiles)
	}

	if summary.Truncated || summary.SymlinksSkipped != 0 || summary.LargeFilesSkipped != 0 {
		t.Errorf("unexpected flags: %+v", summary)
	}
}

func TestDiscover_IgnoredDirectoriesExcludedButNotDeleted(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "main.go"), "package main")
	writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/main")
	writeFile(t, filepath.Join(root, "node_modules/left-pad/index.js"), "module.exports={}")
	writeFile(t, filepath.Join(root, "__pycache__/app.cpython.pyc"), "junk")
	writeFile(t, filepath.Join(root, "vendor/example.com/pkg/pkg.go"), "package pkg")
	writeFile(t, filepath.Join(root, "target/release/binary-artifact"), "junk")
	writeFile(t, filepath.Join(root, "dist/bundle.js"), "junk")
	writeFile(t, filepath.Join(root, "build/output.o"), "junk")
	writeFile(t, filepath.Join(root, "coverage/lcov.info"), "junk")
	writeFile(t, filepath.Join(root, ".next/cache.json"), "junk")
	writeFile(t, filepath.Join(root, "venv/pyvenv.cfg"), "junk")

	service := NewService(Options{})

	summary, err := service.Discover(context.Background(), root)

	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if summary.Files != 1 {
		t.Errorf("Files = %d, want 1 (only main.go; ignored dirs pruned): %+v", summary.Files, summary)
	}

	if summary.Directories != 0 {
		t.Errorf("Directories = %d, want 0 (all present dirs ignored)", summary.Directories)
	}

	// Ignored content must still EXIST on disk.
	for _, kept := range []string{
		"node_modules/left-pad/index.js",
		"__pycache__/app.cpython.pyc",
		"vendor/example.com/pkg/pkg.go",
	} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("ignored content was deleted: %s: %v", kept, err)
		}
	}
}

func TestDiscover_SymlinkEscapeIsNotFollowed(t *testing.T) {
	outside := t.TempDir()

	secret := filepath.Join(outside, "server-secret.txt")

	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatalf("seed outside secret: %v", err)
	}

	root := t.TempDir()

	writeFile(t, filepath.Join(root, "main.go"), "package main")

	if err := os.Symlink(secret, filepath.Join(root, "escape-file.txt")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	service := NewService(Options{})

	summary, err := service.Discover(context.Background(), root)

	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if summary.Files != 1 {
		t.Errorf("Files = %d, want 1 (symlinks never followed/counted)", summary.Files)
	}

	if summary.SymlinksSkipped != 2 {
		t.Errorf("SymlinksSkipped = %d, want 2", summary.SymlinksSkipped)
	}

	if summary.Directories != 0 {
		t.Errorf("Directories = %d, want 0 (symlinked dir pruned, outside tree not entered)", summary.Directories)
	}
}

func TestDiscover_LargeFilesAreSkippedBySizeLimit(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "small.go"), "package main")

	bigPath := filepath.Join(root, "generated.bin")

	if err := os.WriteFile(bigPath, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	service := NewService(Options{MaxFileSize: 1024})

	summary, err := service.Discover(context.Background(), root)

	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if summary.Files != 1 {
		t.Errorf("Files = %d, want 1 (oversized file excluded)", summary.Files)
	}

	if summary.LargeFilesSkipped != 1 {
		t.Errorf("LargeFilesSkipped = %d, want 1", summary.LargeFilesSkipped)
	}
}

func TestDiscover_FileLimitTruncates(t *testing.T) {
	root := t.TempDir()

	for i := 0; i < 25; i++ {
		writeFile(t, filepath.Join(root, "gen"+string(rune('a'+i))+".go"), "package main")
	}

	service := NewService(Options{MaxFiles: 10})

	summary, err := service.Discover(context.Background(), root)

	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}

	if summary.Files != 10 {
		t.Errorf("Files = %d, want exactly MaxFiles", summary.Files)
	}

	if !summary.Truncated {
		t.Errorf("Truncated = false, want true when the limit is hit")
	}
}

func TestDiscover_EmptyAndMissingRoots(t *testing.T) {
	service := NewService(Options{})

	t.Run("empty workspace counts nothing", func(t *testing.T) {
		root := t.TempDir()

		summary, err := service.Discover(context.Background(), root)

		if err != nil {
			t.Fatalf("Discover() error: %v", err)
		}

		if summary.Files != 0 || summary.Directories != 0 || len(summary.Languages) != 0 {
			t.Errorf("empty workspace produced %+v", summary)
		}
	})

	t.Run("missing root is an unreadable-entry error", func(t *testing.T) {
		if _, err := service.Discover(context.Background(), filepath.Join(t.TempDir(), "does-not-exist")); !errors.Is(err, ErrUnreadableEntry) {
			t.Fatalf("error = %v, want ErrUnreadableEntry", err)
		}
	})
}

func TestDiscover_Timeout(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "main.go"), "package main")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	service := NewService(Options{})

	if _, err := service.Discover(ctx, root); !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout on cancelled context", err)
	}

	select {
	case <-time.After(time.Second):
		t.Log("deadline path returned promptly")
	}
}

func TestLanguageForExtension(t *testing.T) {
	cases := map[string]string{
		".go":   "Go",
		".GO":   "Go",
		".tsx":  "TypeScript",
		".PY":   "Python",
		".bash": "Shell",
		"":      "",
		".md":   "",
		".json": "",
		".png":  "",
	}

	for extension, want := range cases {
		got, _ := LanguageForExtension(extension)

		if got != want {
			t.Errorf("languageForExtension(%q) = %q, want %q", extension, got, want)
		}
	}
}
