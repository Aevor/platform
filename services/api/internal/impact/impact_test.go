package impact

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Aevor/platform/services/api/internal/representation"
)

func strPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func hashOf(value string) string {
	sum := 0
	for _, r := range value {
		sum += int(r)
	}
	return fmt.Sprintf("hash-%d", sum)
}

type chunkSpec struct {
	index   int
	symType string
	symName string
	parent  string
	content string
}

// reps emits ONE representation per chunk (matching the real pipeline where a
// representation is derived per chunk), so content concatenation, symbol
// definitions, and import chunks behave exactly like production.
func reps(file, language, role string, chunks ...chunkSpec) []representation.Representation {
	out := make([]representation.Representation, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, representation.Representation{
			ID:           fmt.Sprintf("%s-%d", hashOf(file), chunk.index),
			RepositoryID: "repo-1",
			FilePath:     file,
			FileRole:     role,
			Language:     language,
			ChunkIndex:   chunk.index,
			SymbolType:   chunk.symType,
			SymbolName:   strPtr(chunk.symName),
			ParentSymbol: strPtr(chunk.parent),
			Content:      chunk.content,
			StartLine:    chunk.index + 1,
			EndLine:      chunk.index + 1,
			ContentHash:  fmt.Sprintf("hash-%d-%d", len(chunk.content), chunk.index),
		})
	}
	return out
}

func chunk(index int, symType, symName, content string) chunkSpec {
	return chunkSpec{index: index, symType: symType, symName: symName, content: content}
}

func importsChunk(index int, content string) chunkSpec {
	return chunkSpec{index: index, symType: "imports", content: content}
}

func functionChunk(index int, name, content string) chunkSpec {
	return chunkSpec{index: index, symType: "function", symName: name, content: content}
}

func methodChunk(index int, name, parent, content string) chunkSpec {
	return chunkSpec{index: index, symType: "method", symName: name, parent: parent, content: content}
}

func TestBuildAndAnalyze_ImportsAndCalls(t *testing.T) {
	repositoryID := "repo-1"
	representations := repList(
		withRepo(repositoryID, reps("internal/login/login.go", "Go", "source",
			importsChunk(1, "import \"fmt\"\n"),
			functionChunk(2, "Login", "func Login() {}\n"),
		)),
		withRepo(repositoryID, reps("main.go", "Go", "source",
			importsChunk(1, "import \"github.com/acme/app/internal/login\"\n"),
			functionChunk(2, "main", "func main() {\n\tlogin.Login()\n}\n"),
		)),
	)

	graph := Build(repositoryID, representations)
	analysis := graph.Analyze(Target{FilePath: "internal/login/login.go", Symbol: "Login"}, AnalyzeOptions{})

	if len(analysis.Direct) == 0 {
		t.Fatalf("expected direct impacts, got none")
	}

	called := false
	imported := false
	for _, item := range analysis.Direct {
		if item.FilePath == "main.go" && item.Kind == KindCalls {
			called = true
		}
		if item.FilePath == "main.go" && item.Kind == KindImports {
			imported = true
		}
		if item.Certainty != CertaintyDirect {
			t.Errorf("expected direct certainty, got %q", item.Certainty)
		}
	}
	if !called {
		t.Errorf("main.go should appear as a CALLER of Login; direct=%+v", analysis.Direct)
	}
	if !imported {
		t.Errorf("main.go should appear via an import edge; direct=%+v", analysis.Direct)
	}
}

func TestBuildAndAnalyze_Implements(t *testing.T) {
	repositoryID := "repo-1"
	representations := repList(
		withRepo(repositoryID, reps("pkg/notifier.go", "Go", "source",
			chunk(0, "type_declaration", "Notifier", "type Notifier interface {\n\tNotify()\n}\n"),
		)),
		withRepo(repositoryID, reps("pkg/email.go", "Go", "source",
			chunk(0, "type_declaration", "Emailer", "type Emailer struct{}\n"),
			methodChunk(1, "Notify", "Emailer", "func (e Emailer) Notify() {}\n"),
		)),
	)

	graph := Build(repositoryID, representations)
	analysis := graph.Analyze(Target{FilePath: "pkg/notifier.go", Symbol: "Notifier"}, AnalyzeOptions{})

	found := false
	for _, item := range analysis.Direct {
		if item.FilePath == "pkg/email.go" && item.Kind == KindImplements && item.Certainty == CertaintyInferred {
			found = true
		}
	}
	if !found {
		t.Errorf("expected inferred implements edge from pkg/email.go; direct=%+v", analysis.Direct)
	}
}

func TestBuildAndAnalyze_Tests(t *testing.T) {
	repositoryID := "repo-1"
	mainRep := withRepo(repositoryID, reps("main.go", "Go", "source",
		functionChunk(0, "main", "package main\nfunc main() {}\n")))
	testRep := withRepo(repositoryID, reps("main_test.go", "Go", "test",
		functionChunk(0, "TestMain", "package main\nfunc TestMain(t *testing.T) {}\n")))
	testRep[0].SourceUnderTest = "main.go"

	graph := Build(repositoryID, []representation.Representation{mainRep[0], testRep[0]})
	analysis := graph.Analyze(Target{FilePath: "main.go"}, AnalyzeOptions{})

	foundTest := false
	for _, item := range analysis.Tests {
		if item.FilePath == "main_test.go" {
			foundTest = true
		}
	}
	if !foundTest {
		t.Errorf("expected main_test.go under related tests; tests=%+v", analysis.Tests)
	}
}

func TestBuildAndAnalyze_MultiHop(t *testing.T) {
	repositoryID := "repo-1"
	representations := repList(
		withRepo(repositoryID, reps("pkg/a/a.go", "Go", "source",
			functionChunk(0, "A", "package a\n\nfunc A() {}\n"),
		)),
		withRepo(repositoryID, reps("pkg/b/b.go", "Go", "source",
			importsChunk(0, "import \"github.com/acme/app/pkg/a\"\n"),
			functionChunk(1, "B", "package b\n\nfunc B() {\n\ta.A()\n}\n"),
		)),
		withRepo(repositoryID, reps("pkg/c/c.go", "Go", "source",
			importsChunk(0, "import \"github.com/acme/app/pkg/b\"\n"),
			functionChunk(1, "C", "package c\n\nfunc C() {\n\tb.B()\n}\n"),
		)),
	)

	graph := Build(repositoryID, representations)
	analysis := graph.Analyze(Target{FilePath: "pkg/a/a.go", Symbol: "A"}, AnalyzeOptions{})

	if len(analysis.Direct) == 0 {
		t.Fatalf("expected direct impacts")
	}

	foundIndirect := false
	for _, item := range analysis.Indirect {
		if item.FilePath == "pkg/c/c.go" {
			foundIndirect = true
			if item.Distance != 1 {
				t.Errorf("expected c.go at distance 1, got %d", item.Distance)
			}
		}
	}
	if !foundIndirect {
		t.Errorf("expected pkg/c/c.go as indirect dependant; indirect=%+v", analysis.Indirect)
	}
}

func TestBuildAndAnalyze_NoImpact(t *testing.T) {
	repositoryID := "repo-1"
	isolated := withRepo(repositoryID, reps("pkg/d/d.go", "Go", "source",
		functionChunk(0, "D", "package d\n\nfunc D() {}\n")))

	graph := Build(repositoryID, isolated)
	analysis := graph.Analyze(Target{FilePath: "pkg/d/d.go", Symbol: "D"}, AnalyzeOptions{})

	if len(analysis.Direct) != 0 {
		t.Errorf("expected no direct impacts for isolated file, got %+v", analysis.Direct)
	}
	if len(analysis.Risks) == 0 {
		t.Error("expected a risk noting no repository-derived relationships")
	}
}

func TestBuildAndAnalyze_UnsupportedLanguage(t *testing.T) {
	repositoryID := "repo-1"
	cFile := withRepo(repositoryID, reps("legacy.c", "C", "source",
		functionChunk(0, "Legacy", "int legacy(void) { return 0; }\n")))

	graph := Build(repositoryID, cFile)
	analysis := graph.Analyze(Target{FilePath: "legacy.c", Symbol: "Legacy"}, AnalyzeOptions{})

	if !analysis.Unsupported {
		t.Error("expected unsupported_language true for C language")
	}
	if len(analysis.Unknowns) == 0 {
		t.Error("expected an unknown entry for unsupported language")
	}
	if len(analysis.Risks) == 0 {
		t.Error("expected a risk noting the target language is not structurally analyzed")
	}
}

func TestBuildAndAnalyze_UnknownTarget(t *testing.T) {
	graph := Build("repo-1", nil)
	analysis := graph.Analyze(Target{FilePath: "not/present.go"}, AnalyzeOptions{})

	if len(analysis.Unknowns) == 0 {
		t.Error("expected unknowns for a target outside the repository")
	}
	if len(analysis.Direct) != 0 {
		t.Error("expected no direct impacts for a missing target")
	}
}

func TestBuildAndAnalyze_SymbolNotDefined(t *testing.T) {
	repositoryID := "repo-1"
	file := withRepo(repositoryID, reps("main.go", "Go", "source",
		functionChunk(0, "main", "package main\nfunc main() {}\n")))

	graph := Build(repositoryID, file)
	analysis := graph.Analyze(Target{FilePath: "main.go", Symbol: "Missing"}, AnalyzeOptions{})

	found := false
	for _, unknown := range analysis.Unknowns {
		if unknown == "symbol Missing is not defined in main.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected symbol-not-defined unknown; got %+v", analysis.Unknowns)
	}
}

func TestBuildAndAnalyze_DepthLimits(t *testing.T) {
	repositoryID := "repo-1"
	representations := repList(
		withRepo(repositoryID, reps("l0.go", "Go", "source",
			functionChunk(0, "Svc0", "package l\nfunc Svc0() {}\n"))),
		withRepo(repositoryID, reps("l1.go", "Go", "source",
			functionChunk(0, "Svc1", "package l\nfunc Svc1() { _ = Svc0() }\n"))),
		withRepo(repositoryID, reps("l2.go", "Go", "source",
			functionChunk(0, "Svc2", "package l\nfunc Svc2() { _ = Svc1() }\n"))),
		withRepo(repositoryID, reps("l3.go", "Go", "source",
			functionChunk(0, "Svc3", "package l\nfunc Svc3() { _ = Svc2() }\n"))),
	)

	graph := Build(repositoryID, representations)
	shallow := graph.Analyze(Target{FilePath: "l0.go"}, AnalyzeOptions{Depth: 1})
	deep := graph.Analyze(Target{FilePath: "l0.go"}, AnalyzeOptions{Depth: 3})

	if len(shallow.Indirect) != 1 {
		t.Fatalf("depth 1 should find exactly l2.go as the first hop; got %+v", shallow.Indirect)
	}
	if shallow.Indirect[0].FilePath != "l2.go" {
		t.Errorf("expected l2.go as the single shallow indirect impact, got %s", shallow.Indirect[0].FilePath)
	}
	if len(deep.Indirect) != 2 {
		t.Fatalf("depth 3 should find l2.go and l3.go; got %+v", deep.Indirect)
	}
	wantDepths := map[string]int{"l2.go": 1, "l3.go": 2}
	for _, item := range deep.Indirect {
		if want, ok := wantDepths[item.FilePath]; ok {
			if item.Distance != want {
				t.Errorf("%s at distance %d, want %d", item.FilePath, item.Distance, want)
			}
		} else {
			t.Errorf("unexpected indirect impact %s", item.FilePath)
		}
		if item.Distance > deep.Depth {
			t.Errorf("item distance %d exceeds configured depth %d", item.Distance, deep.Depth)
		}
	}
}

func TestBuild_Deterministic(t *testing.T) {
	repositoryID := "repo-1"
	var representations []representation.Representation
	representations = append(representations,
		withRepo(repositoryID, reps("a.go", "Go", "source",
			functionChunk(0, "A", "package a\nfunc A() { _ = B() }\n")))...)
	representations = append(representations,
		withRepo(repositoryID, reps("b.go", "Go", "source",
			functionChunk(0, "B", "package b\nfunc B() { _ = A() }\n")))...)

	first := Build(repositoryID, representations).Analyze(Target{FilePath: "a.go"}, AnalyzeOptions{})
	second := Build(repositoryID, representations).Analyze(Target{FilePath: "a.go"}, AnalyzeOptions{})

	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("impact analysis is not deterministic:\nfirst: %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestStore_RepositoryIsolation(t *testing.T) {
	store := NewStore(Options{MaxRepositories: 1})

	graphA := Build("repo-a", withRepo("repo-a", reps("one.go", "Go", "source",
		functionChunk(0, "One", "package one\nfunc One() {}\n"))))
	graphB := Build("repo-b", withRepo("repo-b", reps("two.go", "Go", "source",
		functionChunk(0, "Two", "package two\nfunc Two() {}\n"))))

	if err := store.Replace("repo-a", graphA); err != nil {
		t.Fatalf("replace repo-a: %v", err)
	}
	if err := store.Replace("repo-b", graphB); err == nil {
		t.Fatal("expected repository limit error when exceeding MaxRepositories")
	}

	got, ok := store.Get("repo-a")
	if !ok || got == nil || got.RepositoryID() != "repo-a" {
		t.Fatalf("repo-a graph not retrievable")
	}
	// A repo-b target through the repo-a graph can never resolve to repo-b data.
	foreign := got.Analyze(Target{FilePath: "two.go"}, AnalyzeOptions{})
	if len(foreign.Direct) != 0 || len(foreign.Unknowns) == 0 {
		t.Errorf("cross-repository target must not resolve; got %+v", foreign)
	}
}

func TestStore_GetMissingIsOpaque(t *testing.T) {
	store := NewStore(Options{})
	if _, ok := store.Get("repo-nope"); ok {
		t.Fatal("expected missing graph to be absent")
	}
}

func withRepo(repositoryID string, groups ...[]representation.Representation) []representation.Representation {
	var out []representation.Representation
	for _, items := range groups {
		for _, item := range items {
			item.RepositoryID = repositoryID
			out = append(out, item)
		}
	}
	return out
}

func repList(parts ...[]representation.Representation) []representation.Representation {
	var out []representation.Representation
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
