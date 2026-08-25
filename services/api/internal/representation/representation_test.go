package representation

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Aevor/platform/services/api/internal/chunking"
	"github.com/Aevor/platform/services/api/internal/extraction"
)

const repositoryID = "11111111-2222-3333-4444-555555555555"

func extractFile(path string, language string, extension string, content string) extraction.File {
	return extraction.File{
		Path:        path,
		Language:    language,
		Extension:   extension,
		Size:        int64(len(content)),
		Content:     content,
		ContentHash: fmt.Sprintf("hash-%s", path),
	}
}

func chunkResult(files ...[]chunking.Chunk) *chunking.Result {
	result := &chunking.Result{Status: chunking.StatusChunked}

	for _, fileChunks := range files {
		for _, chunk := range fileChunks {
			chunk.RepositoryID = repositoryID
			result.Chunks = append(result.Chunks, chunk)
			result.TotalChunks++
			result.TotalBytes += chunk.ByteSize
		}

		result.Files = append(result.Files, chunking.FileSummary{
			Path:     fileChunks[0].FilePath,
			Language: fileChunks[0].Language,
			Chunks:   len(fileChunks),
			Bytes:    sumBytes(fileChunks),
		})
	}

	return result
}

func sumBytes(chunks []chunking.Chunk) int64 {
	var total int64
	for _, chunk := range chunks {
		total += chunk.ByteSize
	}
	return total
}

// buildFile splits content into deterministic single-line chunks so tests can
// construct controlled chunking results without invoking the chunker.
func buildFile(path string, language string, lines ...string) []chunking.Chunk {
	chunks := make([]chunking.Chunk, 0, len(lines))

	for index, line := range lines {
		content := line + "\n"
		chunks = append(chunks, chunking.Chunk{
			FilePath:    path,
			Language:    language,
			ChunkIndex:  index,
			Content:     content,
			StartLine:   index + 1,
			EndLine:     index + 1,
			ByteSize:    int64(len(content)),
			ContentHash: fmt.Sprintf("%s#%d", path, index),
		})
	}

	return chunks
}

func TestRepresent_NormalSourceChunk(t *testing.T) {
	source := "package main\n\nfunc main() {}\n"
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("main.go", "Go", ".go", source)},
	}
	chunks := chunkResult(buildFile("main.go", "Go",
		"package main",
		"",
		"func main() {}"))

	result := new(Service).Represent(chunks, extracted)

	if result.Status != StatusRepresented {
		t.Fatalf("status = %q, want %q", result.Status, StatusRepresented)
	}

	if len(result.Chunks) != 3 || result.TotalChunks != 3 {
		t.Fatalf("chunks = %d/%d, want 3/3", len(result.Chunks), result.TotalChunks)
	}

	first := result.Chunks[0]
	if first.FileRole != RoleSource {
		t.Errorf("role = %q, want %q", first.FileRole, RoleSource)
	}

	if first.Content != "package main\n" {
		t.Errorf("content = %q, want verbatim source line", first.Content)
	}
}

func TestRepresent_MetadataPreservation(t *testing.T) {
	content := "server:\n  port: 8080\n"
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files: []extraction.File{
			extractFile("cmd/server/main.go", "Go", ".go", content),
		},
	}
	chunks := chunkResult(buildFile("cmd/server/main.go", "Go", "server:", "  port: 8080"))

	result := new(Service).Represent(chunks, extracted)

	got := result.Chunks[0]
	if got.Directory != "cmd/server" {
		t.Errorf("directory = %q, want cmd/server", got.Directory)
	}

	if got.Extension != ".go" {
		t.Errorf("extension = %q, want .go", got.Extension)
	}

	if got.FileSize != int64(len(content)) {
		t.Errorf("file size = %d, want %d", got.FileSize, len(content))
	}

	if got.Language != "Go" {
		t.Errorf("language = %q, want Go", got.Language)
	}

	if got.RepositoryID != repositoryID {
		t.Errorf("repository id = %q, want %q", got.RepositoryID, repositoryID)
	}
}

func TestRepresent_LineNumbersPreserved(t *testing.T) {
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("a.go", "Go", ".go", "one\ntwo\nthree\n")},
	}
	chunks := chunkResult(buildFile("a.go", "Go", "one", "two", "three"))
	result := new(Service).Represent(chunks, extracted)

	for index, representation := range result.Chunks {
		want := index + 1

		if representation.StartLine != want || representation.EndLine != want {
			t.Fatalf("chunk %d lines = [%d,%d], want [%d,%d]",
				index, representation.StartLine, representation.EndLine, want, want)
		}
	}
}

func TestRepresent_FilePathPreserved(t *testing.T) {
	path := "internal/deep/nested/file.go"
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile(path, "Go", ".go", "x\n")},
	}
	chunks := chunkResult(buildFile(path, "Go", "x"))
	result := new(Service).Represent(chunks, extracted)

	if result.Chunks[0].FilePath != path {
		t.Fatalf("path = %q, want %q", result.Chunks[0].FilePath, path)
	}
}

func TestRepresent_LanguagePreserved(t *testing.T) {
	cases := map[string]string{
		"a.go":      "Go",
		"b.py":      "Python",
		"c.ts":      "TypeScript",
		"d.yml":     "",
		"e.unknown": "",
	}

	for path, language := range cases {
		extracted := &extraction.Result{
			Status: extraction.StatusExtracted,
			Files:  []extraction.File{extractFile(path, language, ext(path), "x\n")},
		}
		chunks := chunkResult(buildFile(path, language, "x"))
		result := new(Service).Represent(chunks, extracted)

		if result.Chunks[0].Language != language {
			t.Errorf("%s: language = %q, want %q", path, result.Chunks[0].Language, language)
		}
	}
}

func ext(path string) string {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '.' {
			return path[index:]
		}
	}

	return ""
}

func TestRepresent_SymbolMetadataWhenAvailable(t *testing.T) {
	chunks := chunkResult([]chunking.Chunk{{
		FilePath:     "main.go",
		Language:     "Go",
		ChunkIndex:   0,
		Content:      "func main() {}\n",
		StartLine:    1,
		EndLine:      1,
		ByteSize:     15,
		ContentHash:  "h",
		SymbolName:   "main",
		SymbolType:   "function",
		ParentSymbol: "",
	}})

	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("main.go", "Go", ".go", "func main() {}\n")},
	}

	result := new(Service).Represent(chunks, extracted)
	got := result.Chunks[0]

	if got.SymbolName == nil || *got.SymbolName != "main" {
		t.Fatalf("symbol name = %v, want main", got.SymbolName)
	}

	if got.SymbolType != "function" {
		t.Errorf("symbol type = %q, want function", got.SymbolType)
	}

	if got.ParentSymbol != nil {
		t.Errorf("parent symbol = %v, want nil (absence stays null)", got.ParentSymbol)
	}
}

func TestRepresent_UnknownSymbolBehavior(t *testing.T) {
	chunks := chunkResult(buildFile("styles.css", "", "body { color: red }"))
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("styles.css", "", ".css", "body { color: red }\n")},
	}

	result := new(Service).Represent(chunks, extracted)
	got := result.Chunks[0]

	if got.SymbolName != nil {
		t.Errorf("symbol name = %v, want null for fallback language", got.SymbolName)
	}

	if got.SymbolType != SymbolUnknown {
		t.Errorf("symbol type = %q, want %q", got.SymbolType, SymbolUnknown)
	}

	if got.ParentSymbol != nil {
		t.Errorf("parent symbol = %v, want null", got.ParentSymbol)
	}
}

func TestClassifyRole_FileRoleDetection(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"main.go", RoleSource},
		{"util.py", RoleSource},
		{"app.test.tsx", RoleTest},
		{"user.spec.js", RoleTest},
		{"server_test.go", RoleTest},
		{"tests/test_server.py", RoleTest},
		{".github/workflows/ci.yml", RoleCI},
		{".gitlab-ci.yml", RoleCI},
		{"Jenkinsfile", RoleCI},
		{"go.mod", RoleDependencyManifest},
		{"package.json", RoleDependencyManifest},
		{"Cargo.lock", RoleDependencyManifest},
		{"Makefile", RoleBuild},
		{"Dockerfile.dev", RoleBuild},
		{"README.md", RoleDocumentation},
		{"docs/guide.md", RoleDocumentation},
		{"config/settings.yml", RoleConfiguration},
		{"app.config.json", RoleConfiguration},
		{"assets/logo.png", RoleUnknown},
	}

	for _, testCase := range cases {
		if got := classifyRole(testCase.path); got != testCase.want {
			t.Errorf("classifyRole(%q) = %q, want %q", testCase.path, got, testCase.want)
		}
	}
}

func TestRepresent_DeterministicRepresentation(t *testing.T) {
	build := func() (*chunking.Result, *extraction.Result) {
		files := []extraction.File{
			extractFile("a.go", "Go", ".go", "one\ntwo\n"),
			extractFile("b/b.md", "", ".md", "hello\n"),
		}
		chunks := chunkResult(
			buildFile("a.go", "Go", "one", "two"),
			buildFile("b/b.md", "", "hello"),
		)

		return chunks, &extraction.Result{Status: extraction.StatusExtracted, Files: files}
	}

	chunksA, extractedA := build()
	chunksB, extractedB := build()

	first := new(Service).Represent(chunksA, extractedA)
	second := new(Service).Represent(chunksB, extractedB)

	if !reflect.DeepEqual(first, second) {
		t.Fatal("representations of identical inputs differ")
	}
}

func TestIdentity_DeterministicIdentity(t *testing.T) {
	base := identity(repositoryID, "main.go", 2, "abc")

	if base != identity(repositoryID, "main.go", 2, "abc") {
		t.Fatal("identity not stable for identical inputs")
	}

	if len(base) != 64 {
		t.Fatalf("identity length = %d, want 64 hex chars", len(base))
	}

	variations := []func() string{
		func() string { return identity(repositoryID, "other.go", 2, "abc") },
		func() string { return identity(repositoryID, "main.go", 3, "abc") },
		func() string { return identity(repositoryID, "main.go", 2, "abd") },
		func() string { return identity("99999999-0000-0000-0000-000000000000", "main.go", 2, "abc") },
	}

	seen := map[string]bool{base: true}

	for _, variation := range variations {
		value := variation()
		if seen[value] {
			t.Fatal("identity collision between differing inputs")
		}

		seen[value] = true
	}
}

func TestRepresent_ContentHashPreservation(t *testing.T) {
	chunks := chunkResult(buildFile("a.go", "Go", "one", "two"))
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("a.go", "Go", ".go", "one\ntwo\n")},
	}

	result := new(Service).Represent(chunks, extracted)

	for index, representation := range result.Chunks {
		want := chunks.Chunks[index].ContentHash

		if representation.ContentHash != want {
			t.Fatalf("hash[%d] = %q, want verbatim %q", index, representation.ContentHash, want)
		}

		if representation.Content != chunks.Chunks[index].Content {
			t.Fatalf("content[%d] altered", index)
		}
	}

	// Metadata-only change must not move hashes.
	chunks.TotalBytes += 12345
	altered := new(Service).Represent(chunks, extracted)

	for index := range altered.Chunks {
		if altered.Chunks[index].ContentHash != result.Chunks[index].ContentHash {
			t.Fatalf("hash[%d] changed although content did not", index)
		}
	}
}

func TestRepresent_NeighboringChunkRelationships(t *testing.T) {
	chunks := chunkResult(
		buildFile("a.go", "Go", "one", "two", "three"),
		buildFile("b.md", "", "only"),
	)
	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files: []extraction.File{
			extractFile("a.go", "Go", ".go", "one\ntwo\nthree\n"),
			extractFile("b.md", "", ".md", "only\n"),
		},
	}

	result := new(Service).Represent(chunks, extracted)

	a := result.Chunks
	if a[0].PrevChunkIndex != nil || a[0].NextChunkIndex == nil || *a[0].NextChunkIndex != 1 {
		t.Fatalf("first neighbors wrong: prev=%v next=%v", a[0].PrevChunkIndex, a[0].NextChunkIndex)
	}

	if a[1].PrevChunkIndex == nil || *a[1].PrevChunkIndex != 0 ||
		a[1].NextChunkIndex == nil || *a[1].NextChunkIndex != 2 {
		t.Fatalf("middle neighbors wrong: prev=%v next=%v", a[1].PrevChunkIndex, a[1].NextChunkIndex)
	}

	if a[2].PrevChunkIndex == nil || *a[2].PrevChunkIndex != 1 || a[2].NextChunkIndex != nil {
		t.Fatalf("last neighbors wrong: prev=%v next=%v", a[2].PrevChunkIndex, a[2].NextChunkIndex)
	}

	single := result.Chunks[3]
	if single.PrevChunkIndex != nil || single.NextChunkIndex != nil {
		t.Fatalf("single-chunk file has neighbors: prev=%v next=%v",
			single.PrevChunkIndex, single.NextChunkIndex)
	}
}

func TestRepresent_TestSourceRelationship(t *testing.T) {
	files := []extraction.File{
		extractFile("server.go", "Go", ".go", "package main\n"),
		extractFile("server_test.go", "Go", ".go", "package main\n"),
		extractFile("orphan_test.go", "Go", ".go", "package main\n"), // counterpart missing
		extractFile("calc.py", "Python", ".py", "def add(): pass\n"),
		extractFile("test_calc.py", "Python", ".py", "import calc\n"),
		extractFile("app.test.ts", "TypeScript", ".ts", "export const x = 1;\n"),
		extractFile("app.ts", "TypeScript", ".ts", "export const x = 1;\n"),
	}

	var groups [][]chunking.Chunk

	for _, file := range files {
		groups = append(groups, buildFile(file.Path, file.Language, "line"))
	}

	result := new(Service).Represent(chunkResult(groups...), &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  files,
	})

	byPath := make(map[string]string)

	for _, representation := range result.Chunks {
		byPath[representation.FilePath] = representation.SourceUnderTest
	}

	if byPath["server_test.go"] != "server.go" {
		t.Errorf("server_test.go → %q, want server.go", byPath["server_test.go"])
	}

	if byPath["test_calc.py"] != "calc.py" {
		t.Errorf("test_calc.py → %q, want calc.py", byPath["test_calc.py"])
	}

	if byPath["app.test.ts"] != "app.ts" {
		t.Errorf("app.test.ts → %q, want app.ts", byPath["app.test.ts"])
	}

	if byPath["orphan_test.go"] != "" {
		t.Errorf("orphan_test.go → %q, want empty (counterpart absent)", byPath["orphan_test.go"])
	}

	if byPath["server.go"] != "" {
		t.Errorf("source files must not claim test targets, got %q", byPath["server.go"])
	}
}

func TestRepresent_RepositoryIsolationAndSecurityValidation(t *testing.T) {
	otherID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	chunks := chunkResult(buildFile("a.go", "Go", "one", "two"))

	for i := range chunks.Chunks {
		chunks.Chunks[i].RepositoryID = otherID
	}

	extracted := &extraction.Result{
		Status: extraction.StatusExtracted,
		Files:  []extraction.File{extractFile("a.go", "Go", ".go", "one\ntwo\n")},
	}

	result := new(Service).Represent(chunks, extracted)

	for _, representation := range result.Chunks {
		if representation.RepositoryID != otherID {
			t.Fatalf("repository id = %q, want %q", representation.RepositoryID, otherID)
		}

		identityMatches := identity(otherID, representation.FilePath,
			representation.ChunkIndex, representation.ContentHash) == representation.ID

		if !identityMatches {
			t.Fatal("representation identity is not bound to its owning repository")
		}

		// Security invariants on every emitted path field.
		for _, value := range []string{representation.FilePath, representation.Directory} {
			if len(value) > 0 && value[0] == '/' {
				t.Fatalf("absolute path leaked: %q", value)
			}

			if containsSegment(value, "..") {
				t.Fatalf("traversal segment leaked: %q", value)
			}
		}
	}
}

func containsSegment(value string, segment string) bool {
	for _, part := range splitPath(value) {
		if part == segment {
			return true
		}
	}

	return false
}

func splitPath(value string) []string {
	var parts []string
	current := ""

	for _, char := range value {
		if char == '/' {
			parts = append(parts, current)
			current = ""
			continue
		}

		current += string(char)
	}

	return append(parts, current)
}
