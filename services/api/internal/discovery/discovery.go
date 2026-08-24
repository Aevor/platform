// Package discovery performs READ-ONLY codebase inspection over an Aevor
// repository workspace (Task 3b). It walks the workspace filesystem and
// collects METADATA ONLY — file/directory counts, programming-language
// signals by extension, and important project/configuration files.
//
// Hard security rules, enforced by construction:
//
//   - Repository content is UNTRUSTED input. Nothing is executed: no shell,
//     no repository scripts/hooks/build tools/CI. The walker only stats and
//     lists directory entries.
//   - File contents are NEVER read into memory. All decisions use names,
//     extensions, and Lstat metadata.
//   - Symlinks are never followed (filepath.WalkDir uses Lstat semantics).
//     Symlinked files and symlinked directories are counted as skipped and
//     pruned, so a malicious link can never pull discovery outside the
//     workspace or leak server files into results.
//   - Paths are resolved against the server-controlled workspace root and
//     only RELATIVE paths ever leave this package.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrTimeout is returned when discovery exceeds its deadline. Handlers map
// it to a distinct external error; underlying causes never reach clients.
var ErrTimeout = errors.New("discovery timeout")

// ErrUnreadableEntry wraps unexpected filesystem failures (permission
// errors etc.). It maps to an opaque internal error upstream; the cause
// stays in server logs.
var ErrUnreadableEntry = errors.New("workspace entry unreadable")

const (
	// DefaultMaxFiles bounds how many regular files one discovery considers
	// before marking the result truncated. Metadata-only walking is cheap,
	// but unbounded recursion on adversarial trees is still waste.
	DefaultMaxFiles = 20000

	// DefaultMaxFileSize excludes very large FILES from consideration.
	// Discovery is metadata-only, so this guards future consumers and keeps
	// generated artifacts/blobs from polluting counts.
	DefaultMaxFileSize = 1 << 20 // 1 MiB

	// DefaultTimeout bounds one full traversal.
	DefaultTimeout = time.Minute

	// maxImportantFiles caps the important-file list in the summary.
	maxImportantFiles = 50

	// maxPathLength rejects absurd deep/long entries early.
	maxPathLength = 1024
)

// DefaultIgnoredDirs are directory BASENAMES excluded from discovery.
// Ignored content is NOT deleted and remains in the workspace untouched;
// it is only excluded from the summary. The set covers VCS internals
// (.git), dependency trees (node_modules/vendor), build outputs
// (target/dist/build/out/.next), caches (__pycache__/.gradle), coverage,
// virtual environments (venv/.venv), and editor noise (.idea).
func DefaultIgnoredDirs() map[string]struct{} {
	return map[string]struct{}{
		".git":         {},
		"node_modules": {},
		"vendor":       {},
		"target":       {},
		"dist":         {},
		"build":        {},
		"out":          {},
		".next":        {},
		"coverage":     {},
		"__pycache__":  {},
		".venv":        {},
		"venv":         {},
		".gradle":      {},
		".idea":        {},
	}
}

// Options configures one Service instance. Zero-value fields fall back to
// the package defaults so tests can override individual knobs.
type Options struct {
	MaxFiles    int
	MaxFileSize int64
	IgnoredDirs map[string]struct{}
}

func (o Options) maxFiles() int {
	if o.MaxFiles > 0 {
		return o.MaxFiles
	}

	return DefaultMaxFiles
}

func (o Options) maxFileSize() int64 {
	if o.MaxFileSize > 0 {
		return o.MaxFileSize
	}

	return DefaultMaxFileSize
}

func (o Options) ignoredDirs() map[string]struct{} {
	if len(o.IgnoredDirs) > 0 {
		return o.IgnoredDirs
	}

	return DefaultIgnoredDirs()
}

// Summary is the safe result of one discovery run. It contains ONLY
// aggregate metadata and RELATIVE paths — never absolute paths, file
// contents, or anything tied to the server's filesystem layout.
type Summary struct {
	Files             int
	Directories       int
	Languages         map[string]int
	ImportantFiles    []string
	SymlinksSkipped   int
	LargeFilesSkipped int
	Truncated         bool
}

// Service runs discovery with fixed options. It holds no per-request state
// and is safe for concurrent use.
type Service struct {
	options Options
}

func NewService(options Options) *Service {
	return &Service{options: options}
}

// Discover walks root (a server-controlled workspace directory) and returns
// the aggregate summary. The context deadline bounds the traversal.
func (s *Service) Discover(ctx context.Context, root string) (*Summary, error) {
	absolute, err := filepath.Abs(root)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadableEntry, err)
	}

	summary := &Summary{
		Languages:      make(map[string]int),
		ImportantFiles: make([]string, 0, 8),
	}

	ignored := s.options.ignoredDirs()
	maxFiles := s.options.maxFiles()
	maxFileSize := s.options.maxFileSize()

	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, path, walkErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrTimeout, ctx.Err())
		default:
		}

		relative, err := filepath.Rel(absolute, path)

		if err != nil || len(relative) > maxPathLength {
			return nil //nolint:nilerr // unusable names are skipped, not fatal
		}

		if relative == "." {
			return nil // the workspace root itself is not a "directory" result
		}

		// SECURITY: symlinks are recorded and pruned, never followed. For
		// directories SkipDir prevents descent; for files returning nil
		// simply moves on. WalkDir's Lstat semantics make outside-of-root
		// escape via links impossible here.
		if entry.Type()&fs.ModeSymlink != 0 {
			summary.SymlinksSkipped++

			if entry.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if entry.IsDir() {
			if _, skip := ignored[entry.Name()]; skip {
				return fs.SkipDir
			}

			summary.Directories++

			return nil
		}

		if summary.Files >= maxFiles {
			summary.Truncated = true

			return fs.SkipAll
		}

		info, err := entry.Info()

		if err != nil {
			return fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, path, err)
		}

		if info.Size() > maxFileSize {
			summary.LargeFilesSkipped++

			return nil
		}

		summary.Files++

		name := entry.Name()

		if language, known := LanguageForExtension(filepath.Ext(name)); known {
			summary.Languages[language]++
		}

		if isImportantProjectFile(relative, name) && len(summary.ImportantFiles) < maxImportantFiles {
			summary.ImportantFiles = append(summary.ImportantFiles, relative)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Strings(summary.ImportantFiles)

	return summary, nil
}

// isImportantProjectFile reports whether a relative path is a well-known
// project/configuration marker. Matching is case-insensitive on basenames
// plus explicit .github/workflows detection; no repository layout is
// assumed beyond these conventional names.
func isImportantProjectFile(relativePath string, baseName string) bool {
	lowerRel := strings.ToLower(relativePath)
	lowerBase := strings.ToLower(baseName)

	if strings.HasPrefix(lowerRel, ".github/workflows/") &&
		(strings.HasSuffix(lowerBase, ".yml") || strings.HasSuffix(lowerBase, ".yaml")) {
		return true
	}

	switch lowerBase {
	case "package.json", "go.mod", "cargo.toml", "pom.xml",
		"requirements.txt", "pyproject.toml", "setup.py",
		"composer.json", "gemfile", "makefile", "tsconfig.json",
		"docker-compose.yml", "docker-compose.yaml":
		return true
	}

	for _, prefix := range []string{"readme", "license", "licence", "dockerfile", "docker-compose.", "build.gradle", "settings.gradle"} {
		if strings.HasPrefix(lowerBase, prefix) {
			return true
		}
	}

	return false
}
