// Package extraction turns the files SELECTED by Task 3c filtering into a
// bounded, deterministic, in-memory content representation for future
// codebase processing (Task 3d).
//
// Hard invariants:
//
//   - Only decisions marked Included by filtering are ever read. Binary,
//     secret, generated, junk, and unsupported files never reach this layer.
//   - All reads go through os.Root over the server-controlled workspace:
//     repository-relative paths only, traversal rejected up front, symlink
//     escapes refused by the kernel-assisted root resolution. A leaf that
//     became a symlink after filtering is skipped, never followed.
//   - Nothing is executed, interpreted as code, or installed; file contents
//     are read as opaque bytes with bounded reads.
//   - Content must be valid UTF-8 or it is SKIPPED (reason invalid_utf8).
//     Invalid bytes are never silently replaced — no lossy decoding.
//   - Every extracted file carries a SHA-256 hex digest of its exact bytes.
//     The hash is a change/duplicate detector only, NEVER an authorization
//     mechanism.
//   - Ordering is lexical by relative path; identical workspace state and
//     options produce byte-identical results.
//   - Memory is bounded: extraction inherits the filtering budgets (per-file
//     size, total selected bytes, selected-file count) because it composes
//     the SAME configured filtering.Service, and re-verifies per-file size at
//     open time to close the filter→read race.
//
// File contents are an INTERNAL representation. They are never logged and
// never serialized through HTTP by this package.
package extraction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/Aevor/platform/services/api/internal/filtering"
)

// DefaultTimeout bounds one full extraction run.
const DefaultTimeout = time.Minute

// Sentinels returned by Extract/ExtractDecisions. Handlers map them to
// external error codes; none ever carry file contents.
var (
	// ErrTimeout is returned when extraction exceeds its deadline.
	ErrTimeout = errors.New("extraction timeout")

	// ErrUnreadableEntry wraps unexpected filesystem failures while opening
	// or reading a candidate that filtering had already accepted.
	ErrUnreadableEntry = errors.New("workspace entry unreadable")

	// ErrUnsafePath is returned when a candidate path fails validation.
	// Filtered decisions can never contain such paths, so this indicates a
	// corrupted caller input, not repository state; extraction fails closed.
	ErrUnsafePath = errors.New("unsafe file path")
)

// Skip reasons recorded when an INCLUDED candidate could not be extracted.
// These extend (never contradict) the filtering vocabulary.
const (
	// ReasonNotSelected marks candidates whose decision was not Included.
	// Extract() never produces these; they guard direct ExtractDecisions use.
	ReasonNotSelected = "not_selected"

	// ReasonTooLarge marks files that exceeded the per-file size cap at READ
	// time (e.g. grew between filtering and extraction). Never truncated.
	ReasonTooLarge = "too_large"

	// ReasonSymlink marks leaves that became symlinks between filtering and
	// reading. Symlinks are never followed.
	ReasonSymlink = "symlink"

	// ReasonNotRegularFile marks non-regular leaves (devices, FIFOs, ...).
	ReasonNotRegularFile = "not_regular_file"

	// ReasonInvalidUTF8 marks readable bytes that are not valid UTF-8. No
	// lossy replacement is performed; such content is skipped intact on disk.
	ReasonInvalidUTF8 = "invalid_utf8"

	// ReasonDuplicatePath marks repeated paths in one decision list; the
	// first occurrence wins so the representation stays well-formed.
	ReasonDuplicatePath = "duplicate_path"
)

// StatusExtracted is the Status value of every successful Result.
const StatusExtracted = "extracted"

// File is the internal content representation of one extracted file.
//
// Path is repository-relative with forward slashes. Content holds the EXACT
// file bytes (line endings and BOM preserved) as a Go string; Size is its
// length in bytes. ContentHash is the lowercase hex SHA-256 of those exact
// bytes.
type File struct {
	Path        string
	Language    string
	Extension   string
	Size        int64
	Content     string
	ContentHash string
}

// Result summarizes one extraction run.
//
// Aggregate counts cover every included candidate. Files is COMPLETE (not
// capped): its total size is already bounded by the shared total-bytes
// budget, so downstream consumers always receive all extracted content.
// External surfaces may cap the list themselves.
type Result struct {
	TotalCandidates int            `json:"total_candidates"`
	ExtractedCount  int            `json:"extracted_count"`
	ExtractedBytes  int64          `json:"extracted_bytes"`
	Skipped         map[string]int `json:"skipped"`
	SkippedCount    int            `json:"skipped_count"`
	Files           []File         `json:"-"`
	Complete        bool           `json:"complete"`
	Status          string         `json:"status"`
}

// Options configures extraction. MaxFileSize is the per-file read-time cap;
// selection budgets come from the composed filtering.Service. Zero selects
// DefaultMaxFileSize.
type Options struct {
	MaxFileSize int64
}

// DefaultMaxFileSize mirrors the filtering default: extraction never opens a
// file larger than what filtering would have included.
const DefaultMaxFileSize = filtering.DefaultMaxFileSize

func (o Options) maxFileSize() int64 {
	if o.MaxFileSize <= 0 {
		return DefaultMaxFileSize
	}

	return o.MaxFileSize
}

// Service extracts filtered selections deterministically. It holds no
// per-request state and is safe for concurrent use.
type Service struct {
	filterer    *filtering.Service
	maxFileSize int64
}

// NewService builds an extraction Service over the given filterer. Extraction
// deliberately REUSES the caller's configured filtering.Service so both stages
// enforce identical budgets from one configuration source.
func NewService(filterer *filtering.Service, options Options) *Service {
	return &Service{
		filterer:    filterer,
		maxFileSize: options.maxFileSize(),
	}
}

// Extract filters the workspace and extracts the resulting selection in one
// deterministic pass: filter first (which enforces the shared budgets), then
// read exactly the included files.
func (s *Service) Extract(ctx context.Context, root string) (*Result, error) {
	if s == nil || s.filterer == nil {
		return nil, fmt.Errorf("filtering subsystem is not configured")
	}

	filterResult, err := s.filterer.Filter(ctx, root)

	if err != nil {
		return nil, err
	}

	included := make([]filtering.FileDecision, 0, len(filterResult.Files))

	for _, decision := range filterResult.Files {
		if decision.Included {
			included = append(included, decision)
		}
	}

	return s.ExtractDecisions(ctx, root, included)
}

// ExtractDecisions reads exactly the included decisions under root. It exists
// so hostile or synthetic decision lists can be exercised directly; production
// callers use Extract.
func (s *Service) ExtractDecisions(
	ctx context.Context,
	root string,
	decisions []filtering.FileDecision,
) (*Result, error) {
	if s == nil || s.filterer == nil {
		return nil, fmt.Errorf("filtering subsystem is not configured")
	}

	result := &Result{
		TotalCandidates: len(decisions),
		Skipped:         make(map[string]int),
		Status:          StatusExtracted,
	}
	skip := func(reason string) {
		result.Skipped[reason]++
		result.SkippedCount++
	}

	ordered := make([]filtering.FileDecision, len(decisions))
	copy(ordered, decisions)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Path < ordered[j].Path
	})

	rootFS, err := os.OpenRoot(root)

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadableEntry, err)
	}
	defer func() { _ = rootFS.Close() }()

	var previous string

	for _, decision := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}

		if !decision.Included {
			skip(ReasonNotSelected)
			continue
		}

		if !filtering.ValidRelativePath(decision.Path) {
			return nil, fmt.Errorf("%w", ErrUnsafePath)
		}

		if decision.Path == previous {
			skip(ReasonDuplicatePath)
			continue
		}
		previous = decision.Path

		file, reason, err := s.extractOne(rootFS, decision)

		if err != nil {
			return nil, err
		}

		if reason != "" {
			skip(reason)
			continue
		}

		result.Files = append(result.Files, *file)
		result.ExtractedCount++
		result.ExtractedBytes += file.Size
	}

	result.Complete = result.SkippedCount == 0

	return result, nil
}

// extractOne safely reads ONE included decision and returns either a file,
// a skip reason, or a fatal error. Containment is enforced by rootFS itself:
// Open/Lstat resolve the validated relative path inside the workspace and
// refuse any escape (including symlinked components leaving the tree). The
// explicit Lstat first classifies post-filter leaf swaps cleanly; even if a
// swap races past it, the root resolution still prevents any out-of-tree read.
func (s *Service) extractOne(rootFS *os.Root, decision filtering.FileDecision) (*File, string, error) {
	info, err := rootFS.Lstat(decision.Path)

	if err != nil {
		return nil, "", fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, decision.Path, err)
	}

	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, ReasonSymlink, nil
	case !info.Mode().IsRegular():
		return nil, ReasonNotRegularFile, nil
	}

	handle, err := rootFS.Open(decision.Path)

	if err != nil {
		return nil, "", fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, decision.Path, err)
	}
	defer func() { _ = handle.Close() }()

	stat, err := handle.Stat()

	if err != nil {
		return nil, "", fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, decision.Path, err)
	}

	if stat.Size() > s.maxFileSize {
		return nil, ReasonTooLarge, nil
	}

	// Bounded read: one extra byte detects growth past the cap instead of
	// silently truncating.
	raw, err := io.ReadAll(io.LimitReader(handle, s.maxFileSize+1))

	if err != nil {
		return nil, "", fmt.Errorf("%w: %q: %v", ErrUnreadableEntry, decision.Path, err)
	}

	if int64(len(raw)) > s.maxFileSize {
		return nil, ReasonTooLarge, nil
	}

	if !utf8.Valid(raw) {
		return nil, ReasonInvalidUTF8, nil
	}

	digest := sha256.Sum256(raw)

	return &File{
		Path:        decision.Path,
		Language:    decision.Language,
		Extension:   decision.Extension,
		Size:        int64(len(raw)),
		Content:     string(raw),
		ContentHash: hex.EncodeToString(digest[:]),
	}, "", nil
}
