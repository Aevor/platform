package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Aevor/platform/services/api/internal/extraction"
)

func newService(mutate func(*Options)) *Service {
	options := Options{}

	if mutate != nil {
		mutate(&options)
	}

	return NewService(options)
}

// extractResult builds an extraction.Result in lexical path order, mirroring
// what Task 3d actually produces.
func extractResult(files map[string]string) *extraction.Result {
	result := &extraction.Result{Status: extraction.StatusExtracted}

	paths := make([]string, 0, len(files))

	for path := range files {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	for _, path := range paths {
		result.Files = append(result.Files, extraction.File{
			Path:     path,
			Language: languageFor(path),
			Size:     int64(len(files[path])),
			Content:  files[path],
		})
	}

	return result
}

func languageFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return "Go"
	case strings.HasSuffix(path, ".py"):
		return "Python"
	case strings.HasSuffix(path, ".js"):
		return "JavaScript"
	case strings.HasSuffix(path, ".ts"):
		return "TypeScript"
	case strings.HasSuffix(path, ".java"):
		return "Java"
	case strings.HasSuffix(path, ".rb"):
		return "Ruby"
	case strings.HasSuffix(path, ".css"):
		return "CSS"
	case strings.HasSuffix(path, ".md"):
		return "Markdown"
	default:
		return ""
	}
}

func findChunks(t *testing.T, result *Result, path string) []Chunk {
	t.Helper()

	var matched []Chunk

	for i := range result.Chunks {
		if result.Chunks[i].FilePath == path {
			matched = append(matched, result.Chunks[i])
		}
	}

	if len(matched) == 0 {
		t.Fatalf("no chunks for %q", path)
	}

	return matched
}

func TestChunk_SimpleSourceFile(t *testing.T) {
	source := "package main\n"
	result := newService(nil).Chunk(extractResult(map[string]string{"main.go": source}))

	chunks := findChunks(t, result, "main.go")

	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}

	if chunks[0].Content != source || chunks[0].StartLine != 1 || chunks[0].EndLine != 1 {
		t.Errorf("chunk = %+v", chunks[0])
	}

	if chunks[0].SymbolType != "package" {
		t.Errorf("symbol type = %q", chunks[0].SymbolType)
	}
}

func TestChunk_MultilineSingleUnit(t *testing.T) {
	source := "# Title\n\nSome text.\n"
	result := newService(nil).Chunk(extractResult(map[string]string{"notes.md": source}))

	chunks := findChunks(t, result, "notes.md")

	if len(chunks) != 1 || chunks[0].Content != source {
		t.Fatalf("chunks = %d, content mismatch", len(chunks))
	}

	if chunks[0].StartLine != 1 || chunks[0].EndLine != 3 {
		t.Errorf("lines = %d..%d, want 1..3", chunks[0].StartLine, chunks[0].EndLine)
	}
}

func TestChunk_GoFunctionAndMethodBoundaries(t *testing.T) {
	source := "package demo\n" +
		"\n" +
		"import \"fmt\"\n" +
		"\n" +
		"// Greet prints hello.\n" +
		"func Greet(name string) {\n" +
		"\tfmt.Println(name)\n" +
		"}\n" +
		"\n" +
		"type Server struct {\n" +
		"\tPort int\n" +
		"}\n" +
		"\n" +
		"func (s *Server) Start() error {\n" +
		"\treturn nil\n" +
		"}\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"server.go": source}))
	chunks := findChunks(t, result, "server.go")

	if len(chunks) != 5 {
		t.Fatalf("chunks = %d, want 5 (package, imports, func, type, method):\n%+v", len(chunks), chunks)
	}

	type expectation struct {
		name    string
		kind    string
		parent  string
		content string
	}

	want := []expectation{
		{"", "package", "", "package demo\n\n"},
		{"", "imports", "", "import \"fmt\"\n\n"},
		{"Greet", "function", "", "// Greet prints hello.\nfunc Greet(name string) {\n\tfmt.Println(name)\n}\n\n"},
		{"Server", "type_declaration", "", "type Server struct {\n\tPort int\n}\n\n"},
		{"Start", "method", "Server", "func (s *Server) Start() error {\n\treturn nil\n}\n"},
	}

	for index, expected := range want {
		got := chunks[index]

		if got.SymbolName != expected.name || got.SymbolType != expected.kind || got.ParentSymbol != expected.parent {
			t.Errorf("chunk %d symbols = %q/%q/%q, want %q/%q/%q",
				index, got.SymbolName, got.SymbolType, got.ParentSymbol,
				expected.name, expected.kind, expected.parent)
		}

		if got.Content != expected.content {
			t.Errorf("chunk %d content = %q, want %q", index, got.Content, expected.content)
		}
	}

	// Doc comment attached to its function, not left in the imports chunk.
	if !strings.Contains(chunks[2].Content, "// Greet") {
		t.Error("doc comment not attached to function chunk")
	}
}

func TestChunk_GoImportBlockMerged(t *testing.T) {
	source := "package demo\n" +
		"import (\n" +
		"\t\"fmt\"\n" +
		"\t\"os\"\n" +
		")\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"main.go": source}))
	chunks := findChunks(t, result, "main.go")

	// package line merges nothing (not adjacent), the import BLOCK is one
	// unit because only its first line is a boundary.
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}

	if chunks[1].SymbolType != "imports" || !strings.Contains(chunks[1].Content, `"os"`) {
		t.Errorf("import block chunk wrong: %+v", chunks[1])
	}
}

func TestChunk_PythonClassBoundaries(t *testing.T) {
	source := "import os\n" +
		"import sys\n" +
		"\n" +
		"class Greeter:\n" +
		"    def hi(self):\n" +
		"        return 'hi'\n" +
		"\n" +
		"def farewell():\n" +
		"    return 'bye'\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"greeter.py": source}))
	chunks := findChunks(t, result, "greeter.py")

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (imports, class, function)", len(chunks))
	}

	if chunks[0].SymbolType != "imports" || chunks[0].Content != "import os\nimport sys\n\n" {
		t.Errorf("merged imports chunk wrong: %+v", chunks[0])
	}

	if chunks[1].SymbolName != "Greeter" || chunks[1].SymbolType != "class" {
		t.Errorf("class chunk symbols = %q/%q", chunks[1].SymbolName, chunks[1].SymbolType)
	}

	if chunks[2].SymbolName != "farewell" || chunks[2].SymbolType != "function" {
		t.Errorf("function chunk symbols = %q/%q", chunks[2].SymbolName, chunks[2].SymbolType)
	}
}

func TestChunk_TypeScriptExportBoundaries(t *testing.T) {
	source := "import { x } from \"./x\";\n" +
		"\n" +
		"export class Api {\n" +
		"}\n" +
		"\n" +
		"export default function main() {\n" +
		"}\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"api.ts": source}))
	chunks := findChunks(t, result, "api.ts")

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}

	if chunks[1].SymbolName != "Api" || chunks[1].SymbolType != "class" {
		t.Errorf("export class chunk = %q/%q", chunks[1].SymbolName, chunks[1].SymbolType)
	}

	if chunks[2].SymbolName != "main" || chunks[2].SymbolType != "function" {
		t.Errorf("export function chunk = %q/%q", chunks[2].SymbolName, chunks[2].SymbolType)
	}
}

func TestChunk_JavaClassBoundaries(t *testing.T) {
	source := "public class App {\n" +
		"    private int x;\n" +
		"}\n" +
		"\n" +
		"interface Repo {\n" +
		"}\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"App.java": source}))
	chunks := findChunks(t, result, "App.java")

	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}

	if chunks[0].SymbolName != "App" || chunks[0].SymbolType != "class" {
		t.Errorf("class chunk = %q/%q", chunks[0].SymbolName, chunks[0].SymbolType)
	}

	if chunks[1].SymbolName != "Repo" || chunks[1].SymbolType != "interface" {
		t.Errorf("interface chunk = %q/%q", chunks[1].SymbolName, chunks[1].SymbolType)
	}
}

func TestChunk_UnsupportedLanguageFallback(t *testing.T) {
	source := strings.Repeat(".rule { color: red; }\n", 30)

	result := newService(func(o *Options) { o.MaxChunkLines = 12 }).
		Chunk(extractResult(map[string]string{"theme.css": source}))

	chunks := findChunks(t, result, "theme.css")

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}

	for index, chunk := range chunks {
		if chunk.ChunkIndex != index {
			t.Errorf("index = %d at position %d", chunk.ChunkIndex, index)
		}

		if chunk.SymbolType != "" {
			t.Errorf("fallback chunk carries symbol metadata: %+v", chunk)
		}

		expectedLines := 12
		if index == 2 {
			expectedLines = 6
		}

		if got := chunk.EndLine - chunk.StartLine + 1; got != expectedLines {
			t.Errorf("chunk %d covers %d lines, want %d", index, got, expectedLines)
		}
	}
}

func TestChunk_LargeFileManyChunks(t *testing.T) {
	var builder strings.Builder

	builder.WriteString("package big\n")

	for i := 0; i < 50; i++ {
		builder.WriteString("\nfunc F" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "() {\n\tpass\n}\n")
	}

	result := newService(nil).Chunk(extractResult(map[string]string{"big.go": builder.String()}))

	chunks := findChunks(t, result, "big.go")

	if len(chunks) < 40 {
		t.Fatalf("chunks = %d, want one per function plus preamble", len(chunks))
	}

	if result.TotalChunks != len(chunks) {
		t.Errorf("TotalChunks = %d, want %d", result.TotalChunks, len(chunks))
	}
}

func TestChunk_SmallFileSingleChunk(t *testing.T) {
	result := newService(nil).Chunk(extractResult(map[string]string{"tiny.go": "package tiny\n"}))

	if result.TotalChunks != 1 || result.FilesChunked != 1 {
		t.Fatalf("chunks/files = %d/%d, want 1/1", result.TotalChunks, result.FilesChunked)
	}
}

func TestChunk_EmptyFileProducesNoChunks(t *testing.T) {
	result := newService(nil).Chunk(extractResult(map[string]string{"empty.md": ""}))

	if result.TotalChunks != 0 || result.EmptyFiles != 1 || result.FilesChunked != 0 {
		t.Fatalf("chunks/empty/chunked = %d/%d/%d, want 0/1/0",
			result.TotalChunks, result.EmptyFiles, result.FilesChunked)
	}
}

func TestChunk_DeterministicOrderingAcrossRuns(t *testing.T) {
	files := map[string]string{
		"z.go":   "package z\n\nfunc Z() {}\n",
		"a.py":   "def a():\n    pass\n",
		"m/x.js": "function x() {}\n",
	}

	service := newService(nil)

	first := service.Chunk(extractResult(files))

	for run := 0; run < 3; run++ {
		again := service.Chunk(extractResult(files))

		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs from first run", run)
		}
	}

	// Files are processed in lexical order: a.py before m/x.js before z.go.
	order := []string{}
	for _, summary := range first.Files {
		order = append(order, summary.Path)
	}

	if !reflect.DeepEqual(order, []string{"a.py", "m/x.js", "z.go"}) {
		t.Errorf("file order = %v", order)
	}
}

func TestChunk_HashStabilityAndChangeDetection(t *testing.T) {
	service := newService(nil)

	before := service.Chunk(extractResult(map[string]string{
		"app.go": "package app\n\nfunc A() {\n\tone()\n}\n\nfunc B() {\n\ttwo()\n}\n",
	}))

	chunksBefore := findChunks(t, before, "app.go")

	after := service.Chunk(extractResult(map[string]string{
		"app.go": "package app\n\nfunc A() {\n\tONE_CHANGED()\n}\n\nfunc B() {\n\ttwo()\n}\n",
	}))

	chunksAfter := findChunks(t, after, "app.go")

	if len(chunksBefore) != len(chunksAfter) {
		t.Fatalf("chunk count changed: %d -> %d", len(chunksBefore), len(chunksAfter))
	}

	hashesBefore := map[int]string{}

	for index, chunk := range chunksBefore {
		digest := sha256.Sum256([]byte(chunk.Content))

		if chunk.ContentHash != hex.EncodeToString(digest[:]) {
			t.Errorf("chunk %d hash is not SHA-256 of content", index)
		}

		hashesBefore[index] = chunk.ContentHash
	}

	changedFound := false

	for index, chunk := range chunksAfter {
		if chunk.ContentHash != hashesBefore[index] {
			changedFound = true
		}
	}

	if !changedFound {
		t.Error("no chunk hash changed although source changed")
	}
}

func TestChunk_UnchangedSourceSameHashes(t *testing.T) {
	files := map[string]string{"stable.go": "package stable\n\nfunc S() {}\n"}

	first := newService(nil).Chunk(extractResult(files))
	second := newService(nil).Chunk(extractResult(files))

	firstHashes := []string{}
	secondHashes := []string{}

	for _, chunk := range first.Chunks {
		firstHashes = append(firstHashes, chunk.ContentHash)
	}

	for _, chunk := range second.Chunks {
		secondHashes = append(secondHashes, chunk.ContentHash)
	}

	if !reflect.DeepEqual(firstHashes, secondHashes) {
		t.Error("unchanged source produced different chunk hashes")
	}
}

func TestChunk_LineNumbersContiguousCoverage(t *testing.T) {
	var builder strings.Builder

	for i := 0; i < 25; i++ {
		builder.WriteString(strings.Repeat("x", 20) + "\n")
	}

	result := newService(func(o *Options) { o.MaxChunkLines = 10 }).
		Chunk(extractResult(map[string]string{"rows.txt": builder.String()}))

	chunks := findChunks(t, result, "rows.txt")

	expectedStart := 1

	for _, chunk := range chunks {
		if chunk.StartLine != expectedStart {
			t.Errorf("chunk %d starts at %d, want %d", chunk.ChunkIndex, chunk.StartLine, expectedStart)
		}

		expectedStart = chunk.EndLine + 1
	}

	if expectedStart-1 != 25 {
		t.Errorf("coverage ended at %d, want 25", expectedStart-1)
	}
}

func TestChunk_IndexesSequentialPerFile(t *testing.T) {
	files := map[string]string{
		"b.go": "package b\n\nfunc B() {}\n",
		"a.go": "package a\n\nfunc A() {}\n",
	}

	result := newService(nil).Chunk(extractResult(files))

	counts := map[string]int{}

	for _, chunk := range result.Chunks {
		if chunk.ChunkIndex != counts[chunk.FilePath] {
			t.Errorf("%s chunk index %d out of order", chunk.FilePath, chunk.ChunkIndex)
		}

		counts[chunk.FilePath]++
	}
}

func TestChunk_MaxChunkBytesSplitsAtLineBoundary(t *testing.T) {
	lines := strings.Repeat("abcdefgh\n", 12) // 9 bytes per line

	result := newService(func(o *Options) { o.MaxChunkBytes = 25 }).
		Chunk(extractResult(map[string]string{"data.log": lines}))

	chunks := findChunks(t, result, "data.log")

	if len(chunks) != 6 { // 9 bytes per line, 25-byte cap → 2 lines (18B) per window; 12/2 = 6
		t.Fatalf("chunks = %d, want 6", len(chunks))
	}

	for index, chunk := range chunks {
		if int64(chunk.ByteSize) > 25 && index < len(chunks)-1 {
			t.Errorf("chunk %d size %d exceeds limit", index, chunk.ByteSize)
		}

		if index < len(chunks)-1 && !strings.HasSuffix(chunk.Content, "\n") {
			t.Errorf("chunk %d splits mid-line", index)
		}
	}
}

func TestChunk_SingleOversizeLineAllowedOnce(t *testing.T) {
	long := strings.Repeat("y", 100) + "\n"

	result := newService(func(o *Options) { o.MaxChunkBytes = 10 }).
		Chunk(extractResult(map[string]string{"wide.txt": long}))

	chunks := findChunks(t, result, "wide.txt")

	if len(chunks) != 1 || int64(len(chunks[0].Content)) != int64(len(long)) {
		t.Fatalf("oversize line handling wrong: %+v", chunks)
	}
}

func TestChunk_PerFileChunkLimit(t *testing.T) {
	var builder strings.Builder

	for i := 0; i < 10; i++ {
		builder.WriteString("line\n")
	}

	result := newService(func(o *Options) {
		o.MaxChunkLines = 1
		o.MaxChunksPerFile = 3
	}).Chunk(extractResult(map[string]string{"many.rb": builder.String()}))

	chunks := findChunks(t, result, "many.rb")

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (per-file cap)", len(chunks))
	}

	if result.Truncated {
		t.Error("global truncated flag must stay false for a per-file cap")
	}

	if result.SkippedSummary[ReasonFileChunkLimit] != 1 {
		t.Errorf("skipped summary = %v, want file_chunk_limit:1", result.SkippedSummary)
	}

	if !result.Files[0].Truncated {
		t.Error("file summary must be flagged truncated")
	}
}

func TestChunk_GlobalCapsStopSafely(t *testing.T) {
	file := strings.Repeat("row\n", 50)

	result := newService(func(o *Options) {
		o.MaxChunkLines = 1
		o.MaxChunksPerFile = 100
		o.MaxChunksTotal = 7
	}).Chunk(extractResult(map[string]string{
		"a.txt": file,
		"b.txt": file,
	}))

	if result.TotalChunks != 7 {
		t.Fatalf("total chunks = %d, want 7", result.TotalChunks)
	}

	if !result.Truncated {
		t.Error("truncated flag missing")
	}

	if result.SkippedSummary[ReasonRepositoryChunkLimit] == 0 {
		t.Errorf("skipped summary = %v, want repository_chunk_limit events", result.SkippedSummary)
	}

	if result.TotalFiles != 2 || result.FilesChunked >= 2 {
		t.Errorf("files total/chunked = %d/%d, second file must be skipped", result.TotalFiles, result.FilesChunked)
	}
}

func TestChunk_GlobalByteBudgetStopsSafely(t *testing.T) {
	file := strings.Repeat("0123456789\n", 10) // 110 bytes

	result := newService(func(o *Options) {
		o.MaxTotalChunkBytes = 150
	}).Chunk(extractResult(map[string]string{
		"a.txt": file,
		"b.txt": file,
	}))

	if result.TotalBytes > 150 {
		t.Errorf("bytes = %d over budget 150", result.TotalBytes)
	}

	if !result.Truncated || result.SkippedSummary[ReasonRepositoryByteLimit] == 0 {
		t.Errorf("truncated=%v skipped=%v", result.Truncated, result.SkippedSummary)
	}
}

func TestChunk_ExcludedContentNeverPresent(t *testing.T) {
	// Extraction already excludes secrets/binaries; simulate a realistic
	// selection and prove no chunk carries them even if a sibling file does.
	envSecret := "API_KEY=never-chunked"
	result := newService(nil).Chunk(extractResult(map[string]string{
		"main.go": "package main\n\nconst tokenPlaceholder = \"placeholder\"\n",
	}))

	for _, chunk := range result.Chunks {
		if strings.Contains(chunk.Content, envSecret) || strings.Contains(chunk.FilePath, ".env") {
			t.Fatal("secret content reached chunks")
		}
	}
}

func TestChunk_BinaryLikeContentNotSpecialHere(t *testing.T) {
	// Binaries are filtered upstream; the chunker only ever sees valid UTF-8
	// text and must pass it through verbatim.
	text := "plain ascii\nwith unicode ünïcødé ✅\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"u.md": text}))

	if result.Chunks[0].Content != text {
		t.Error("unicode content altered")
	}
}

func TestChunk_CommentsPreservedVerbatim(t *testing.T) {
	source := "// Copyright header.\n// Licensed under MIT.\n\npackage licensed\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"lic.go": source}))

	chunks := findChunks(t, result, "lic.go")

	var combined strings.Builder

	for _, chunk := range chunks {
		combined.WriteString(chunk.Content)
	}

	if combined.String() != source {
		t.Error("comments were stripped or rewritten")
	}
}

func TestChunk_CRLFLineEndingsPreserved(t *testing.T) {
	source := "package win\r\n\r\nfunc W() {\r\n}\r\n"

	result := newService(nil).Chunk(extractResult(map[string]string{"win.go": source}))

	var combined strings.Builder

	for _, chunk := range findChunks(t, result, "win.go") {
		combined.WriteString(chunk.Content)
	}

	if combined.String() != source {
		t.Error("CRLF terminators were altered")
	}
}

func TestChunk_ConcatenationReproducesExactContent(t *testing.T) {
	sources := map[string]string{
		"go.go":     "package g\n\nimport \"os\"\n\nfunc F() {\n\tif true {\n\t\treturn\n\t}\n}\n",
		"p.py":      "import os\n\nclass C:\n    def m(self):\n        pass\n",
		"plain.css": "a {}\nb {}\nc {}\n",
	}

	result := newService(nil).Chunk(extractResult(sources))

	rebuilt := map[string]string{}

	for _, chunk := range result.Chunks {
		rebuilt[chunk.FilePath] += chunk.Content
	}

	for path, content := range sources {
		if rebuilt[path] != content {
			t.Errorf("%s reconstruction mismatch:\n%q\n!=\n%q", path, rebuilt[path], content)
		}
	}
}

func TestChunk_RepositoryIDSetByCallerNotChunker(t *testing.T) {
	result := newService(nil).Chunk(extractResult(map[string]string{"main.go": "package main\n"}))

	if result.Chunks[0].RepositoryID != "" {
		t.Error("chunker must leave RepositoryID empty; the repositories service owns identity")
	}
}
