package indexing

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/Aevor/platform/services/api/internal/representation"
)

const (
	repositoryOne = "11111111-1111-1111-1111-111111111111"
	repositoryTwo = "22222222-2222-2222-2222-222222222222"
)

func testRepresentation(repositoryID string, filePath string, chunkIndex int) representation.Representation {
	symbol := "Run"
	parent := "Server"
	previous := chunkIndex - 1
	next := chunkIndex + 1

	item := representation.Representation{
		ID:              fmt.Sprintf("%s:%s:%d", repositoryID, filePath, chunkIndex),
		RepositoryID:    repositoryID,
		FilePath:        filePath,
		Directory:       "internal",
		Extension:       ".go",
		FileSize:        128,
		FileRole:        representation.RoleSource,
		Language:        "Go",
		ChunkIndex:      chunkIndex,
		StartLine:       chunkIndex + 1,
		EndLine:         chunkIndex + 1,
		ByteSize:        16,
		ContentHash:     fmt.Sprintf("hash:%s:%d", filePath, chunkIndex),
		SymbolName:      &symbol,
		SymbolType:      "method",
		ParentSymbol:    &parent,
		PrevChunkIndex:  &previous,
		NextChunkIndex:  &next,
		SourceUnderTest: "",
	}

	if chunkIndex == 0 {
		item.PrevChunkIndex = nil
	}

	return item
}

func mustReplace(t *testing.T, index *Index, repositoryID string, items ...representation.Representation) {
	t.Helper()

	if err := index.Replace(repositoryID, items); err != nil {
		t.Fatalf("Replace(%q) error = %v", repositoryID, err)
	}
}

func TestIndex_NormalRepositoryAndMetadataOnlyRecords(t *testing.T) {
	index := New(Options{})
	item := testRepresentation(repositoryOne, "internal/server.go", 0)
	item.Content = "package internal\n"

	mustReplace(t, index, repositoryOne, item)

	got := index.Lookup(Query{RepositoryID: repositoryOne})
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1", len(got))
	}

	if got[0].ID != item.ID || got[0].ContentHash != item.ContentHash ||
		got[0].StartLine != item.StartLine || got[0].EndLine != item.EndLine {
		t.Fatalf("record did not preserve representation metadata: %#v", got[0])
	}

	if _, exists := reflect.TypeOf(Record{}).FieldByName("Content"); exists {
		t.Fatal("metadata index Record must not retain representation content")
	}
}

func TestIndex_MultipleFilesAndRepositoryFileLookup(t *testing.T) {
	index := New(Options{})
	mustReplace(t, index, repositoryOne,
		testRepresentation(repositoryOne, "cmd/main.go", 0),
		testRepresentation(repositoryOne, "internal/server.go", 0),
		testRepresentation(repositoryOne, "internal/server.go", 1),
	)

	if got, want := index.Files(repositoryOne), []string{"cmd/main.go", "internal/server.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Files() = %#v, want %#v", got, want)
	}

	got := index.Lookup(Query{RepositoryID: repositoryOne, FilePath: "internal/server.go"})
	if len(got) != 2 || got[0].ChunkIndex != 0 || got[1].ChunkIndex != 1 {
		t.Fatalf("file lookup = %#v, want two sequential server chunks", got)
	}
}

func TestIndex_MetadataLookupsAndPathPrefix(t *testing.T) {
	index := New(Options{})
	goItem := testRepresentation(repositoryOne, "internal/server.go", 0)
	pythonItem := testRepresentation(repositoryOne, "pkg/worker.py", 1)
	pythonItem.Language = "Python"
	pythonItem.Extension = ".py"
	pythonItem.FileRole = representation.RoleTest
	name := "work"
	pythonItem.SymbolName = &name
	pythonItem.SymbolType = "function"

	mustReplace(t, index, repositoryOne, goItem, pythonItem)

	cases := []struct {
		name  string
		query Query
		want  string
	}{
		{"language", Query{RepositoryID: repositoryOne, Language: "Python"}, "pkg/worker.py"},
		{"role", Query{RepositoryID: repositoryOne, FileRole: representation.RoleTest}, "pkg/worker.py"},
		{"symbol", Query{RepositoryID: repositoryOne, SymbolName: "work"}, "pkg/worker.py"},
		{"symbol type", Query{RepositoryID: repositoryOne, SymbolType: "method"}, "internal/server.go"},
		{"content hash", Query{RepositoryID: repositoryOne, ContentHash: goItem.ContentHash}, "internal/server.go"},
		{"chunk index", Query{RepositoryID: repositoryOne, ChunkIndex: intPointer(0)}, "internal/server.go"},
		{"path prefix", Query{RepositoryID: repositoryOne, PathPrefix: "pkg/"}, "pkg/worker.py"},
		{"combined", Query{RepositoryID: repositoryOne, Language: "Go", PathPrefix: "internal/"}, "internal/server.go"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := index.Lookup(testCase.query)
			if len(got) != 1 || got[0].FilePath != testCase.want {
				t.Fatalf("Lookup(%#v) = %#v, want %q", testCase.query, got, testCase.want)
			}
		})
	}
}

func TestIndex_DeterministicOrderingAndRecordCopies(t *testing.T) {
	index := New(Options{})
	items := []representation.Representation{
		testRepresentation(repositoryOne, "b.go", 1),
		testRepresentation(repositoryOne, "a.go", 1),
		testRepresentation(repositoryOne, "a.go", 0),
	}

	mustReplace(t, index, repositoryOne, items...)
	first := index.Lookup(Query{RepositoryID: repositoryOne})
	second := index.Lookup(Query{RepositoryID: repositoryOne})

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical lookup results differ: %#v != %#v", first, second)
	}

	if got, want := []string{first[0].FilePath, first[1].FilePath, first[2].FilePath},
		[]string{"a.go", "a.go", "b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("file order = %#v, want %#v", got, want)
	}
	if first[0].ChunkIndex != 0 || first[1].ChunkIndex != 1 {
		t.Fatalf("chunk order = %d, %d; want 0, 1", first[0].ChunkIndex, first[1].ChunkIndex)
	}

	*first[0].SymbolName = "mutated"
	if got := index.Lookup(Query{RepositoryID: repositoryOne, SymbolName: "Run"}); len(got) != 3 {
		t.Fatalf("mutating a returned pointer changed indexed data: %#v", got)
	}
}

func TestIndex_ReplaceHandlesUnchangedChangedDeletedAndNewFiles(t *testing.T) {
	index := New(Options{})
	unchanged := testRepresentation(repositoryOne, "keep.go", 0)
	deleted := testRepresentation(repositoryOne, "delete.go", 0)
	mustReplace(t, index, repositoryOne, unchanged, deleted)

	changed := testRepresentation(repositoryOne, "keep.go", 0)
	changed.ID = "changed-identity"
	changed.ContentHash = "changed-hash"
	newFile := testRepresentation(repositoryOne, "new.go", 0)
	mustReplace(t, index, repositoryOne, changed, newFile)

	if got := index.Lookup(Query{RepositoryID: repositoryOne, ContentHash: unchanged.ContentHash}); len(got) != 0 {
		t.Fatalf("unchanged prior content hash still indexed after change: %#v", got)
	}
	if got := index.Lookup(Query{RepositoryID: repositoryOne, ContentHash: deleted.ContentHash}); len(got) != 0 {
		t.Fatalf("deleted file still indexed: %#v", got)
	}
	if got := index.Lookup(Query{RepositoryID: repositoryOne, FilePath: "keep.go"}); len(got) != 1 || got[0].ID != changed.ID {
		t.Fatalf("changed file lookup = %#v, want changed identity", got)
	}
	if got := index.Lookup(Query{RepositoryID: repositoryOne, FilePath: "new.go"}); len(got) != 1 || got[0].ID != newFile.ID {
		t.Fatalf("new file lookup = %#v, want new file", got)
	}

	// Replacing with identical representations preserves the already-derived
	// deterministic identity and produces the same metadata result.
	before := index.Lookup(Query{RepositoryID: repositoryOne})
	mustReplace(t, index, repositoryOne, changed, newFile)
	after := index.Lookup(Query{RepositoryID: repositoryOne})
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unchanged replacement altered records: %#v != %#v", before, after)
	}
}

func TestIndex_EmptyRepositoryAndRemove(t *testing.T) {
	index := New(Options{})
	mustReplace(t, index, repositoryOne)

	if got := index.Files(repositoryOne); len(got) != 0 {
		t.Fatalf("empty repository files = %#v, want empty", got)
	}
	if got := index.Lookup(Query{RepositoryID: repositoryOne}); len(got) != 0 {
		t.Fatalf("empty repository records = %#v, want empty", got)
	}

	mustReplace(t, index, repositoryOne, testRepresentation(repositoryOne, "main.go", 0))
	index.Remove(repositoryOne)
	if got := index.Lookup(Query{RepositoryID: repositoryOne}); len(got) != 0 {
		t.Fatalf("removed repository records = %#v, want empty", got)
	}
}

func TestIndex_LimitsAndDuplicateConsistency(t *testing.T) {
	index := New(Options{MaxRepositories: 1, MaxEntriesPerRepository: 2, MaxEntriesTotal: 2})
	first := testRepresentation(repositoryOne, "first.go", 0)
	second := testRepresentation(repositoryOne, "second.go", 0)
	third := testRepresentation(repositoryOne, "third.go", 0)

	if err := index.Replace(repositoryOne, []representation.Representation{first, second, third}); !errors.Is(err, ErrEntryLimit) {
		t.Fatalf("large snapshot error = %v, want %v", err, ErrEntryLimit)
	}
	mustReplace(t, index, repositoryOne, first, second)

	if err := index.Replace(repositoryTwo, []representation.Representation{testRepresentation(repositoryTwo, "two.go", 0)}); !errors.Is(err, ErrRepositoryLimit) {
		t.Fatalf("repository limit error = %v, want %v", err, ErrRepositoryLimit)
	}

	duplicate := first
	if err := New(Options{}).Replace(repositoryOne, []representation.Representation{first, duplicate}); !errors.Is(err, ErrDuplicateRepresentation) {
		t.Fatalf("duplicate error = %v, want %v", err, ErrDuplicateRepresentation)
	}
}

func TestIndex_RepositoryIsolationAndInputValidation(t *testing.T) {
	index := New(Options{})
	mustReplace(t, index, repositoryOne, testRepresentation(repositoryOne, "one.go", 0))
	mustReplace(t, index, repositoryTwo, testRepresentation(repositoryTwo, "two.go", 0))

	if got := index.Lookup(Query{RepositoryID: repositoryOne}); len(got) != 1 || got[0].RepositoryID != repositoryOne {
		t.Fatalf("repository one lookup leaked data: %#v", got)
	}
	if got := index.Lookup(Query{RepositoryID: repositoryTwo}); len(got) != 1 || got[0].RepositoryID != repositoryTwo {
		t.Fatalf("repository two lookup leaked data: %#v", got)
	}
	if got := index.Lookup(Query{RepositoryID: ""}); len(got) != 0 {
		t.Fatalf("missing repository identity returned records: %#v", got)
	}

	mismatch := testRepresentation(repositoryTwo, "wrong.go", 0)
	if err := index.Replace(repositoryOne, []representation.Representation{mismatch}); !errors.Is(err, ErrRepositoryMismatch) {
		t.Fatalf("mixed repository error = %v, want %v", err, ErrRepositoryMismatch)
	}

	unsafe := testRepresentation(repositoryOne, "../escape.go", 0)
	if err := index.Replace(repositoryOne, []representation.Representation{unsafe}); !errors.Is(err, ErrInvalidRepresentation) {
		t.Fatalf("unsafe path error = %v, want %v", err, ErrInvalidRepresentation)
	}
}

func TestIndex_ConcurrentReplacementAndLookup(t *testing.T) {
	index := New(Options{})
	first := []representation.Representation{testRepresentation(repositoryOne, "first.go", 0)}
	second := []representation.Representation{testRepresentation(repositoryOne, "second.go", 0)}
	mustReplace(t, index, repositoryOne, first...)

	const workers = 16
	errors := make(chan error, workers*2)
	var group sync.WaitGroup

	for worker := 0; worker < workers; worker++ {
		group.Add(2)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 100; iteration++ {
				if err := index.Replace(repositoryOne, first); err != nil {
					errors <- err
				}
			}
		}()
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 100; iteration++ {
				records := index.Lookup(Query{RepositoryID: repositoryOne})
				if len(records) != 1 || records[0].RepositoryID != repositoryOne {
					errors <- fmt.Errorf("inconsistent lookup result: %#v", records)
				}
			}
		}()
	}

	group.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	// A second valid snapshot remains an atomic replacement after concurrent use.
	mustReplace(t, index, repositoryOne, second...)
	if got := index.Lookup(Query{RepositoryID: repositoryOne}); len(got) != 1 || got[0].FilePath != "second.go" {
		t.Fatalf("final replacement = %#v, want second snapshot", got)
	}
}

func intPointer(value int) *int {
	return &value
}
