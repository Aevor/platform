// Package representation enriches Task 3e chunks with traceable file and
// symbol metadata for the future embedding/AI layers (Task 3f).
//
// Architectural invariants:
//
//   - PURE transformation over chunking + extraction results: ZERO filesystem
//     access; every security property is inherited from Tasks 3a–3e by
//     construction. Only authorized repository data can be represented.
//   - CONSERVATIVE METADATA: nothing is invented. Symbols are copied verbatim
//     from chunking (null/unknown when absent), roles come from deterministic
//     name/path rules, and the base source/config/documentation role REUSES
//     the existing filtering.Classify policy so purpose never contradicts
//     inclusion decisions.
//   - EXACT CONTENT: chunk content and content hashes are copied verbatim.
//     Metadata never influences ContentHash — a metadata-only change leaves
//     every content hash unchanged.
//   - DETERMINISTIC IDENTITY: Representation.ID is the hex SHA-256 of
//     repository_id, file_path, chunk_index and content_hash joined with
//     newlines. No timestamps, no random UUIDs; the same repository state
//     yields byte-identical representations, including order (lexical files,
//     sequential chunks — inherited from extraction/chunking).
//   - TRACEABILITY: repository → file → chunk → source lines is fully
//     addressable via RepositoryID / FilePath / Directory / ChunkIndex /
//     StartLine / EndLine plus Prev/Next neighbor indexes within each file.
package representation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/extraction"
	"github.com/Aevor/platform/services/api/internal/filtering"
)

// File roles annotate the analysis PURPOSE of a file. They are deliberately
// distinct from filtering's selection categories; the base mapping reuses the
// shared filtering policy so purpose and inclusion never disagree.
const (
	RoleSource             = "source"
	RoleTest               = "test"
	RoleConfiguration      = "configuration"
	RoleDocumentation      = "documentation"
	RoleDependencyManifest = "dependency_manifest"
	RoleBuild              = "build"
	RoleCI                 = "ci"
	RoleUnknown            = "unknown"

	// StatusRepresented is the Status value of every successful Result.
	StatusRepresented = "represented"

	// SymbolUnknown is reported when no reliable symbol information exists
	// (fallback languages, non-boundary chunks). SymbolName and ParentSymbol
	// use JSON null instead, keeping absence and "unknown" distinguishable.
	SymbolUnknown = "unknown"
)

// Result summarizes one representation run. Chunks is COMPLETE for the
// processed work; external surfaces may cap the list themselves.
type Result struct {
	TotalFiles  int
	TotalChunks int
	TotalBytes  int64
	RoleCounts  map[string]int
	Chunks      []Representation
	Status      string
}

// Representation is the traceable unit consumed by future stages.
//
// Pointer fields marshal to JSON null when the information does not exist;
// they are never guessed values.
type Representation struct {
	// Deterministic identity.
	ID           string
	RepositoryID string

	// File context.
	FilePath  string // repository-relative slash path, verbatim from extraction
	Directory string // repository-relative directory ("." for root)
	Extension string // includes the leading dot; empty when none
	FileSize  int64  // extracted file size in bytes
	FileRole  string // one of the Role* constants
	Language  string // discovery label; empty when unknown upstream

	// Chunk context.
	ChunkIndex int
	StartLine  int // 1-based inclusive
	EndLine    int // 1-based inclusive
	ByteSize   int64

	// Neighboring chunks within the same file; null at file boundaries.
	PrevChunkIndex *int
	NextChunkIndex *int

	// Deterministic test→source association. Set ONLY on chunks of a test
	// file whose conventional counterpart exists among the represented
	// files; empty otherwise. Never inferred semantically.
	SourceUnderTest string

	// Best-effort symbol information, copied verbatim from chunking.
	SymbolName   *string // null when unknown
	SymbolType   string  // SymbolUnknown when unknown
	ParentSymbol *string // null when absent

	// Exact chunk content, verbatim from Task 3e.
	Content     string
	ContentHash string
}

// Service derives representations from chunks. Stateless and safe for
// concurrent use.
type Service struct{}

// NewService builds a representation Service.
func NewService() *Service {
	return &Service{}
}

// Represent transforms chunked content into the enriched representation. It
// never fails: inputs are validated and bounded upstream, so undeterminable
// metadata becomes null/unknown rather than an error or an invention.
func (s *Service) Represent(chunks *chunking.Result, extracted *extraction.Result) *Result {
	fileMeta := make(map[string]extraction.File, len(extracted.Files))

	for _, file := range extracted.Files {
		fileMeta[file.Path] = file
	}

	indexesByFile := make(map[string][]int)

	for i := range chunks.Chunks {
		chunk := &chunks.Chunks[i]
		indexesByFile[chunk.FilePath] = append(indexesByFile[chunk.FilePath], i)
	}

	testTargets := testSourceTargets(chunks.Files)
	roleCounts := make(map[string]int)
	result := &Result{
		TotalFiles: len(chunks.Files),
		RoleCounts: roleCounts,
		Status:     StatusRepresented,
	}

	for _, summary := range chunks.Files {
		position := indexesByFile[summary.Path]

		for positionIndex, chunkIndex := range position {
			chunk := &chunks.Chunks[chunkIndex]
			representation := Representation{
				RepositoryID: chunk.RepositoryID,
				FilePath:     chunk.FilePath,
				Directory:    directoryOf(chunk.FilePath),
				Extension:    extensionOf(fileMeta, chunk.FilePath),
				FileSize:     fileSizeOf(fileMeta, chunk.FilePath),
				FileRole:     classifyRole(chunk.FilePath),
				Language:     chunk.Language,
				ChunkIndex:   chunk.ChunkIndex,
				StartLine:    chunk.StartLine,
				EndLine:      chunk.EndLine,
				ByteSize:     chunk.ByteSize,
				SymbolType:   SymbolUnknown,
				Content:      chunk.Content,
				ContentHash:  chunk.ContentHash,
			}

			if chunk.SymbolName != "" {
				name := chunk.SymbolName
				representation.SymbolName = &name
			}

			if chunk.SymbolType != "" {
				representation.SymbolType = chunk.SymbolType
			}

			if chunk.ParentSymbol != "" {
				parent := chunk.ParentSymbol
				representation.ParentSymbol = &parent
			}

			if positionIndex > 0 {
				previous := position[positionIndex-1]
				representation.PrevChunkIndex = &previous
			}

			if positionIndex < len(position)-1 {
				following := position[positionIndex+1]
				representation.NextChunkIndex = &following
			}

			representation.SourceUnderTest = testTargets[chunk.FilePath]
			representation.ID = identity(
				representation.RepositoryID,
				representation.FilePath,
				representation.ChunkIndex,
				representation.ContentHash,
			)

			result.Chunks = append(result.Chunks, representation)
			result.TotalBytes += representation.ByteSize
			result.RoleCounts[representation.FileRole]++
		}

		result.TotalChunks += summary.Chunks
	}

	return result
}

// classifyRole determines the analysis purpose of a file. Refinement rules
// are checked most-specific-first (case-insensitive on the base name); the
// base category comes from the SHARED filtering policy.
func classifyRole(relativePath string) string {
	base := strings.ToLower(path.Base(relativePath))

	// CI pipelines: workflow directories and known pipeline entry points.
	if strings.HasPrefix(relativePath, ".github/workflows/") ||
		relativePath == ".circleci/config.yml" ||
		strings.HasSuffix(base, ".gitlab-ci.yml") || base == "jenkinsfile" ||
		base == ".travis.yml" || base == "azure-pipelines.yml" {
		return RoleCI
	}

	// Dependency manifests and lockfiles.
	switch base {
	case "go.mod", "go.sum", "package.json", "package-lock.json",
		"yarn.lock", "pnpm-lock.yaml", "cargo.toml", "cargo.lock",
		"pom.xml", "gemfile", "gemfile.lock", "composer.json",
		"requirements.txt", "pipfile", "pipfile.lock", "pyproject.toml":
		return RoleDependencyManifest
	}

	// Build orchestration.
	if base == "makefile" || base == "dockerfile" ||
		strings.HasPrefix(base, "dockerfile.") ||
		base == "cmakelists.txt" || base == "justfile" ||
		base == "build.gradle" || strings.HasPrefix(base, "build.gradle.") ||
		base == "build.bazel" {
		return RoleBuild
	}

	if isTestFile(relativePath) {
		return RoleTest
	}

	switch filtering.Classify(relativePath, 0, 0).Category {
	case filtering.CategoryDocs:
		return RoleDocumentation
	case filtering.CategoryConfig:
		return RoleConfiguration
	case filtering.CategorySource:
		return RoleSource
	default:
		return RoleUnknown
	}
}

// isTestFile recognizes conventional test naming per language family only;
// anything else is not a test.
func isTestFile(relativePath string) bool {
	base := strings.ToLower(path.Base(relativePath))

	switch {
	case strings.HasSuffix(base, "_test.go"): // Go convention
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"): // pytest convention
		return true
	case strings.HasSuffix(base, "_test.py"): // unittest convention
		return true
	default:
		return hasSuffixAny(base, ".test.js", ".test.jsx", ".test.ts", ".test.tsx",
			".spec.js", ".spec.jsx", ".spec.ts", ".spec.tsx")
	}
}

// testSourceTargets maps test-file paths to their conventional source
// counterparts, but ONLY when that counterpart exists among the chunked
// files. Unsupported conventions simply have no entry.
func testSourceTargets(summaries []chunking.FileSummary) map[string]string {
	present := make(map[string]bool, len(summaries))
	for _, summary := range summaries {
		present[summary.Path] = true
	}

	targets := make(map[string]string)

	for _, summary := range summaries {
		directory := path.Dir(summary.Path)
		base := strings.ToLower(path.Base(summary.Path))
		candidate := ""

		switch {
		case strings.HasSuffix(base, "_test.go"):
			stem := strings.TrimSuffix(base, "_test.go")
			candidate = joinRelative(directory, stem+".go")
		case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
			stem := strings.TrimSuffix(strings.TrimPrefix(base, "test_"), ".py")
			candidate = joinRelative(directory, stem+".py")
		case strings.HasSuffix(base, "_test.py"):
			stem := strings.TrimSuffix(base, "_test.py")
			candidate = joinRelative(directory, stem+".py")
		case strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".spec.js"):
			stem := strings.TrimSuffix(strings.TrimSuffix(base, ".js"), ".spec")
			stem = strings.TrimSuffix(stem, ".test")
			candidate = joinRelative(directory, stem+".js")
		case strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".spec.ts"):
			stem := strings.TrimSuffix(strings.TrimSuffix(base, ".ts"), ".spec")
			stem = strings.TrimSuffix(stem, ".test")
			candidate = joinRelative(directory, stem+".ts")
		}

		if candidate != "" && present[candidate] {
			targets[summary.Path] = candidate
		}
	}

	return targets
}

func hasSuffixAny(value string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}

	return false
}

func joinRelative(directory string, base string) string {
	if directory == "." || directory == "" {
		return base
	}

	return directory + "/" + base
}

func directoryOf(relativePath string) string {
	return path.Dir(relativePath)
}

func extensionOf(fileMeta map[string]extraction.File, filePath string) string {
	if file, ok := fileMeta[filePath]; ok {
		return file.Extension
	}

	return ""
}

func fileSizeOf(fileMeta map[string]extraction.File, filePath string) int64 {
	if file, ok := fileMeta[filePath]; ok {
		return file.Size
	}

	return 0
}

// identity derives the deterministic representation ID from stable
// identifiers only. Newline joining is collision-safe here: validator-checked
// relative paths cannot contain control characters, chunk indexes are
// integers, and content hashes are fixed-length lowercase hex.
func identity(repositoryID string, filePath string, chunkIndex int, contentHash string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%s",
		repositoryID, filePath, chunkIndex, contentHash)))

	return hex.EncodeToString(digest[:])
}
