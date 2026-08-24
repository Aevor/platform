package filtering

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/Aevor/platform/services/api/internal/discovery"
)

// Sentinel errors mapped by handlers to distinct external codes; underlying
// causes never reach clients.
var (
	// ErrTimeout is returned when filtering exceeds its deadline.
	ErrTimeout = errors.New("filtering timeout")

	// ErrUnreadableEntry wraps unexpected filesystem failures (permission
	// errors etc.). It maps to an opaque internal error upstream; the cause
	// stays in server logs.
	ErrUnreadableEntry = errors.New("workspace entry unreadable")
)

const (
	// DefaultMaxFileSize is the per-file inclusion limit. A file larger than
	// this is EXCLUDED with reason too_large — never silently truncated.
	DefaultMaxFileSize = 1 << 20 // 1 MiB

	// DefaultMaxTotalBytes bounds the total size of SELECTED content so a
	// downstream consumer can never be handed an unbounded payload.
	DefaultMaxTotalBytes = 32 << 20 // 32 MiB

	// DefaultMaxSelectedFiles bounds how many files may be INCLUDED.
	DefaultMaxSelectedFiles = 5000

	// DefaultTimeout bounds one full traversal.
	DefaultTimeout = time.Minute

	// maxDetailedFiles caps the per-file decision list carried in the result.
	// Counts stay exact for every candidate; only the DETAIL listing is capped
	// (deterministically, after sorting) so huge repositories cannot explode
	// response payloads.
	maxDetailedFiles = 1000

	// maxPathLength rejects absurd deep/long entries early (mirrors discovery).
	maxPathLength = 1024
)

// Options configures one Service instance. Zero-value fields fall back to the
// package defaults so tests can override individual knobs. The values are
// wired from environment configuration in cmd/server (FILTER_MAX_*).
type Options struct {
	MaxFileSize      int64
	MaxTotalBytes    int64
	MaxSelectedFiles int
	IgnoredDirs      map[string]struct{}
}

func (o Options) maxFileSize() int64 {
	if o.MaxFileSize > 0 {
		return o.MaxFileSize
	}

	return DefaultMaxFileSize
}

func (o Options) maxTotalBytes() int64 {
	if o.MaxTotalBytes > 0 {
		return o.MaxTotalBytes
	}

	return DefaultMaxTotalBytes
}

func (o Options) maxSelectedFiles() int {
	if o.MaxSelectedFiles > 0 {
		return o.MaxSelectedFiles
	}

	return DefaultMaxSelectedFiles
}

func (o Options) ignoredDirs() map[string]struct{} {
	if len(o.IgnoredDirs) > 0 {
		return o.IgnoredDirs
	}

	// Single source of truth shared with discovery: one ignore policy, two
	// consumers (aggregate statistics and per-file selection).
	return discovery.DefaultIgnoredDirs()
}

// FileDecision is the explicit, observable outcome for ONE candidate file:
// repository-relative path, metadata, category, and the reason for its
// inclusion or exclusion. No file content ever appears here.
type FileDecision struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Extension string `json:"extension"`
	Language  string `json:"language"`
	Category  string `json:"category"`
	Included  bool   `json:"included"`
	Reason    string `json:"reason"`
}

// Result summarizes one filtering run. Aggregate counts cover EVERY candidate;
// Files carries at most maxDetailedFiles decisions (sorted by path), flagged
// by FilesTruncated.
type Result struct {
	TotalFiles         int            `json:"total_files"`
	IncludedFiles      int            `json:"included_files"`
	ExcludedFiles      int            `json:"excluded_files"`
	TotalSelectedBytes int64          `json:"total_selected_bytes"`
	Languages          map[string]int `json:"languages"`
	ExclusionSummary   map[string]int `json:"exclusion_summary"`
	Files              []FileDecision `json:"files"`
	FilesTruncated     bool           `json:"files_truncated"`
	IgnoredDirectories int            `json:"ignored_directories"`
	SymlinksSkipped    int            `json:"symlinks_skipped"`
	Truncated          bool           `json:"truncated"`
	Status             string         `json:"status"`
}

// Service runs filtering with fixed options. It holds no per-request state
// and is safe for concurrent use.
type Service struct {
	options Options
}

func NewService(options Options) *Service {
	return &Service{options: options}
}

// Filter walks root (a server-controlled workspace directory previously
// verified by workspace.Manager.Ready) and returns the explicit selection
// decisions. The context deadline bounds the traversal.
//
// .gitignore is deliberately NOT consulted: Git ignore rules express build
// hygiene, not AI-analysis suitability, and honoring them would make the
// selection depend on patterns this service does not control. The policy is
// exactly the tables in filter.go — explicit, deterministic, testable.
func (s *Service) Filter(ctx context.Context, root string) (*Result, error) {
	absolute, err := filepath.Abs(root)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadableEntry, err)
	}

	result := &Result{
		Languages:        make(map[string]int),
		ExclusionSummary: make(map[string]int),
		Files:            make([]FileDecision, 0, 16),
		Status:           "filtered",
	}

	ignored := s.options.ignoredDirs()
	maxFileSize := s.options.maxFileSize()
	maxTotalBytes := s.options.maxTotalBytes()
	maxSelected := s.options.maxSelectedFiles()

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
			return nil // the workspace root itself is not a candidate
		}

		relative = filepath.ToSlash(relative)

		// SECURITY: symlinks are counted and pruned, never followed. For
		// directories SkipDir prevents descent; WalkDir's Lstat semantics
		// make outside-of-root escape via links impossible here.
		if entry.Type()&fs.ModeSymlink != 0 {
			result.SymlinksSkipped++

			if entry.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if entry.IsDir() {
			if _, skip := ignored[entry.Name()]; skip {
				result.IgnoredDirectories++
				result.ExclusionSummary[ReasonIgnoredDirectory]++

				return fs.SkipDir
			}

			return nil
		}

		info, err := entry.Info()

		if err != nil {
			return fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, path, err)
		}

		// Defense in depth: the walker cannot emit an unsafe relative path,
		// but a hostile entry name must never reach decisions or responses.
		if !ValidRelativePath(relative) {
			result.ExclusionSummary[ReasonUnsupported]++
			result.ExcludedFiles++

			return nil
		}

		result.TotalFiles++

		decision := Classify(relative, info.Size(), maxFileSize)

		switch {
		case decision.Included && result.IncludedFiles >= maxSelected:
			// Selection budget exhausted: report precisely instead of
			// silently dropping the candidate.
			decision.Included = false
			decision.Category = ""
			decision.Reason = ReasonFileCountLimit
			result.Truncated = true
		case decision.Included && result.TotalSelectedBytes+info.Size() > maxTotalBytes:
			// Size-budget semantics: the running total of SELECTED bytes can
			// never exceed MaxTotalBytes. A candidate that would push the
			// total over the budget is excluded with a clear reason; a LATER
			// candidate small enough to still fit remains selectable. The
			// outcome is deterministic because the traversal order is
			// deterministic (lexical WalkDir + sorted decision list).
			decision.Included = false
			decision.Category = ""
			decision.Reason = ReasonTotalSizeLimit
			result.Truncated = true
		case decision.Included:
			result.TotalSelectedBytes += info.Size()

			if decision.Language != "" {
				result.Languages[decision.Language]++
			}
		}

		if decision.Included {
			result.IncludedFiles++
		} else {
			result.ExcludedFiles++
			result.ExclusionSummary[decision.Reason]++
		}

		if len(result.Files) < maxDetailedFiles {
			result.Files = append(result.Files, FileDecision{
				Path:      relative,
				Size:      info.Size(),
				Extension: decision.Extension,
				Language:  decision.Language,
				Category:  decision.Category,
				Included:  decision.Included,
				Reason:    decision.Reason,
			})
		} else {
			result.FilesTruncated = true
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(result.Files, func(i, j int) bool {
		return result.Files[i].Path < result.Files[j].Path
	})

	return result, nil
}
