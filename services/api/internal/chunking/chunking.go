// Package chunking splits the files produced by Task 3d extraction into
// meaningful, bounded, deterministic chunks for the future representation and
// embedding layers (Task 3e).
//
// Architectural invariants:
//
//   - PURE transformation: Chunk consumes an extraction.Result and touches no
//     filesystem. Every security property (path validation, traversal/symlink
//     refusal, ownership, size bounds) is inherited from Tasks 3a–3d by
//     construction — a chunk can only originate from an authorized workspace.
//   - STRUCTURAL first: for Go, Python, JavaScript/TypeScript, and Java,
//     top-level declaration lines (column 0 keyword starts) become chunk
//     boundaries with best-effort symbol metadata. This is deliberate
//     heuristic segmentation, NOT an AST framework; unusual formatting can
//     shift a boundary but content is always preserved verbatim.
//   - FALLBACK: every other language uses deterministic line-window packing
//     that never splits mid-line.
//   - EXACTNESS: chunk content is the exact substring of the extracted file
//     content spanning its lines (line endings and BOM preserved); hashes are
//     SHA-256 over those exact bytes. Comments are never stripped or
//     rewritten; leading comment/annotation runs directly above a structural
//     boundary travel WITH that boundary (conventional doc-comment placement).
//   - DETERMINISM: identical inputs produce identical chunks — same order
//     (files lexical, inherited from extraction; chunks sequential),
//     boundaries, indexes, and hashes.
//   - BOUNDED: per-chunk line/byte limits, per-file and global chunk-count
//     caps, and a global byte budget. When a cap is hit the result is flagged
//     and remaining work is skipped deterministically; the server cannot be
//     exhausted by a large repository.
//   - NO OVERLAP: chunks do not overlap. Precise start/end line metadata lets
//     any consumer re-read context deterministically instead of duplicating
//     source; overlap would complicate hash-based change detection for zero
//     architectural benefit today.
package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Aevor/platform/services/api/internal/extraction"
)

// Skip reasons recorded when limits truncate work. Counts are EVENTS (a
// truncated file, a file skipped entirely), never contents.
const (
	// ReasonFileChunkLimit marks files that hit MaxChunksPerFile.
	ReasonFileChunkLimit = "file_chunk_limit"

	// ReasonRepositoryChunkLimit marks files skipped because the global
	// chunk-count cap was reached.
	ReasonRepositoryChunkLimit = "repository_chunk_limit"

	// ReasonRepositoryByteLimit marks files skipped because the global byte
	// budget was reached.
	ReasonRepositoryByteLimit = "repository_byte_limit"
)

// StatusChunked is the Status value of every successful Result.
const StatusChunked = "chunked"

// Defaults respect the Task 3c/3d envelope (≤1 MiB per file, ≤5000 files,
// 32 MiB total selected): worst-case chunk metadata stays small and content
// memory is capped by the byte budget regardless of chunk count.
const (
	// DefaultMaxChunkLines bounds the lines of one chunk.
	DefaultMaxChunkLines = 200

	// DefaultMaxChunkBytes bounds the bytes of one chunk.
	DefaultMaxChunkBytes = 16 << 10 // 16 KiB

	// DefaultMaxChunksPerFile bounds chunks produced from one file.
	DefaultMaxChunksPerFile = 500

	// DefaultMaxChunksTotal bounds chunks across the whole operation.
	DefaultMaxChunksTotal = 20000

	// DefaultMaxTotalChunkBytes bounds summed chunk bytes globally.
	DefaultMaxTotalChunkBytes = 32 << 20 // 32 MiB
)

// Options configures chunking; zero fields select the defaults above.
type Options struct {
	MaxChunkLines      int
	MaxChunkBytes      int64
	MaxChunksPerFile   int
	MaxChunksTotal     int
	MaxTotalChunkBytes int64
}

func (o Options) maxChunkLines() int {
	if o.MaxChunkLines <= 0 {
		return DefaultMaxChunkLines
	}

	return o.MaxChunkLines
}

func (o Options) maxChunkBytes() int64 {
	if o.MaxChunkBytes <= 0 {
		return DefaultMaxChunkBytes
	}

	return o.MaxChunkBytes
}

func (o Options) maxChunksPerFile() int {
	if o.MaxChunksPerFile <= 0 {
		return DefaultMaxChunksPerFile
	}

	return o.MaxChunksPerFile
}

func (o Options) maxChunksTotal() int {
	if o.MaxChunksTotal <= 0 {
		return DefaultMaxChunksTotal
	}

	return o.MaxChunksTotal
}

func (o Options) maxTotalChunkBytes() int64 {
	if o.MaxTotalChunkBytes <= 0 {
		return DefaultMaxTotalChunkBytes
	}

	return o.MaxTotalChunkBytes
}

// Chunk is the internal representation of one segmented piece of one file.
//
// Content is the exact substring of the extracted file spanning
// [StartLine, EndLine] (both 1-based, inclusive). RepositoryID is populated
// by the repositories service layer; the chunker itself is repository-
// agnostic. SymbolName/SymbolType/ParentSymbol are BEST-EFFORT and empty
// whenever unreliable.
type Chunk struct {
	RepositoryID string
	FilePath     string
	Language     string
	ChunkIndex   int
	Content      string
	StartLine    int
	EndLine      int
	ByteSize     int64
	ContentHash  string
	SymbolName   string
	SymbolType   string
	ParentSymbol string
}

// FileSummary records what happened to one file without repeating content.
type FileSummary struct {
	Path      string
	Language  string
	Chunks    int
	Bytes     int64
	Truncated bool
}

// Result summarizes one chunking run. Chunks is COMPLETE for the processed
// work; Truncated/SkippedSummary report what was left out and why.
type Result struct {
	TotalFiles     int
	FilesChunked   int
	EmptyFiles     int
	TotalChunks    int
	TotalBytes     int64
	Chunks         []Chunk
	Files          []FileSummary
	Truncated      bool
	SkippedSummary map[string]int
	Status         string
}

// Service chunks extracted files with fixed options. Stateless and safe for
// concurrent use.
type Service struct {
	options Options
}

// NewService builds a chunking Service.
func NewService(options Options) *Service {
	return &Service{options: options}
}

// Chunk transforms extracted files into chunks. It never fails: inputs are
// already validated and bounded upstream, so any limit overflow becomes a
// deterministic truncation recorded on the Result.
func (s *Service) Chunk(source *extraction.Result) *Result {
	result := &Result{
		TotalFiles:     len(source.Files),
		SkippedSummary: make(map[string]int),
		Status:         StatusChunked,
	}

	maxLines := s.options.maxChunkLines()
	maxBytes := s.options.maxChunkBytes()
	maxPerFile := s.options.maxChunksPerFile()
	maxTotal := s.options.maxChunksTotal()
	maxTotalBytes := s.options.maxTotalChunkBytes()

	skip := func(reason string) {
		result.SkippedSummary[reason]++
	}

	for _, file := range source.Files {
		if file.Content == "" {
			result.EmptyFiles++
			continue
		}

		if result.TotalChunks >= maxTotal || result.TotalBytes >= maxTotalBytes {
			result.Truncated = true
			skip(ReasonRepositoryChunkLimit)

			continue
		}

		chunksBefore := result.TotalChunks
		bytesBefore := result.TotalBytes
		fileTruncated := false

		lines := splitLines(file.Content)

		var units []unit

		if rules := rulesFor(file.Language); rules != nil {
			units = buildUnits(rules, lines)
		}

		if units == nil {
			units = []unit{{start: 0, endExcl: len(lines)}}
		}

		fileChunkCount := 0

		for _, current := range units {
			windows := splitWindow(lines[current.start:current.endExcl], maxLines, maxBytes)

			for windowIndex, window := range windows {
				if result.TotalChunks >= maxTotal {
					result.Truncated = true
					skip(ReasonRepositoryChunkLimit)
					fileTruncated = true

					break
				}

				if result.TotalBytes+int64(len(window)) > maxTotalBytes {
					result.Truncated = true
					skip(ReasonRepositoryByteLimit)
					fileTruncated = true

					break
				}

				if fileChunkCount >= maxPerFile {
					skip(ReasonFileChunkLimit)
					fileTruncated = true

					break
				}

				digest := sha256.Sum256([]byte(window))

				meta := current.meta

				if windowIndex > 0 {
					// Continuation windows describe the same region but the
					// symbol was already reported on the first window.
					meta = unitMeta{}
				}

				offset := windowOffset(windows, windowIndex)

				result.Chunks = append(result.Chunks, Chunk{
					FilePath:     file.Path,
					Language:     file.Language,
					ChunkIndex:   fileChunkCount,
					Content:      window,
					StartLine:    current.start + offset + 1,
					EndLine:      current.start + offset + countLines(window),
					ByteSize:     int64(len(window)),
					ContentHash:  hex.EncodeToString(digest[:]),
					SymbolName:   meta.symbolName,
					SymbolType:   meta.symbolType,
					ParentSymbol: meta.parentSymbol,
				})

				result.TotalChunks++
				result.TotalBytes += int64(len(window))
				fileChunkCount++
			}

			if fileTruncated {
				break
			}
		}

		if result.TotalChunks > chunksBefore {
			result.FilesChunked++

			result.Files = append(result.Files, FileSummary{
				Path:      file.Path,
				Language:  file.Language,
				Chunks:    result.TotalChunks - chunksBefore,
				Bytes:     result.TotalBytes - bytesBefore,
				Truncated: fileTruncated,
			})
		}
	}

	return result
}

// splitLines splits content into lines, KEEPING each line's terminator so
// concatenation reproduces the original bytes exactly. The final segment has
// no terminator when the content does not end with one.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}

	parts := strings.SplitAfter(content, "\n")

	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}

	return parts
}

// countLines reports how many logical lines a window covers. Windows are
// built from whole line segments, so only the file-final unterminated line
// needs special handling.
func countLines(window string) int {
	if window == "" {
		return 0
	}

	count := strings.Count(window, "\n")

	if !strings.HasSuffix(window, "\n") {
		count++
	}

	return count
}

// windowOffset sums the line counts of all earlier windows so chunk line
// numbers stay correct after a hard split.
func windowOffset(windows []string, index int) int {
	total := 0

	for _, window := range windows[:index] {
		total += countLines(window)
	}

	return total
}

// splitWindow slices whole line segments into consecutive windows obeying the
// line and byte limits. A single line longer than the byte limit forms its
// own oversize chunk (documented exception — mid-line splits would corrupt
// content).
func splitWindow(lines []string, maxLines int, maxBytes int64) []string {
	windows := make([]string, 0, len(lines)/maxLines+1)

	for start := 0; start < len(lines); {
		end := start + 1
		size := int64(len(lines[start]))

		for end < len(lines) &&
			end-start < maxLines &&
			size+int64(len(lines[end])) <= maxBytes {
			size += int64(len(lines[end]))
			end++
		}

		windows = append(windows, strings.Join(lines[start:end], ""))
		start = end
	}

	return windows
}
