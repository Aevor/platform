package repoinfo

import (
	"encoding/json"
	"path"
	"reflect"
	"strconv"
	"testing"

	"github.com/Aevor/platform/services/api/internal/representation"
)

func str(value string) *string { return &value }

func rep(filePath string, language string, role string, symbols [][2]string, content string, chunkIndex int) representation.Representation {
	directory := path.Dir(filePath)
	if directory == "." {
		directory = "."
	}

	item := representation.Representation{
		ID:           "id_" + filePath,
		RepositoryID: "repo-id",
		FilePath:     filePath,
		Directory:    directory,
		Extension:    path.Ext(filePath),
		FileSize:     int64(len(content)),
		FileRole:     role,
		Language:     language,
		ChunkIndex:   chunkIndex,
		StartLine:    1,
		EndLine:      1 + chunkIndex,
		ByteSize:     int64(len(content)),
		SymbolType:   representation.SymbolUnknown,
		Content:      content,
		ContentHash:  "hash_" + filePath + "_" + strconv.Itoa(chunkIndex),
	}

	if len(symbols) > 0 {
		item.SymbolType = symbols[0][1]
		item.SymbolName = str(symbols[0][0])
	}

	return item
}

func sourceRep(filePath string, language string, symbols [][2]string, content string) representation.Representation {
	return rep(filePath, language, representation.RoleSource, symbols, content, 0)
}

func TestDiscoverAggregates(t *testing.T) {
	chunks := []representation.Representation{
		sourceRep("cmd/api/main.go", "Go", [][2]string{{"main", "package"}}, "package main"),
		sourceRep("cmd/api/handlers.go", "Go", [][2]string{{"API", "type_declaration"}}, "package api"),
		sourceRep("internal/pkg/foo.go", "Go", [][2]string{{"Foo", "function"}}, "package pkg"),
		rep("internal/pkg/foo_test.go", "Go", representation.RoleTest, [][2]string{{"TestFoo", "function"}}, "package pkg", 0),
		rep("internal/pkg/bar_test.go", "Go", representation.RoleTest, [][2]string{{"TestBar", "function"}}, "package pkg", 0),
		rep("openapi.yaml", "YAML", representation.RoleConfiguration, nil, "openapi: 3.0.0", 0),
	}

	profile := Discover(chunks, DefaultLimits())

	if profile.Structure.Files != 6 {
		t.Fatalf("expected 6 files, got %d", profile.Structure.Files)
	}
	if profile.Structure.Chunks != 6 {
		t.Fatalf("expected 6 chunks, got %d", profile.Structure.Chunks)
	}
	if profile.Structure.LanguageCounts["Go"] != 5 {
		t.Fatalf("expected 5 Go files, got %d", profile.Structure.LanguageCounts["Go"])
	}
	if profile.Structure.RoleCounts[representation.RoleTest] != 2 {
		t.Fatalf("expected 2 test files, got %d", profile.Structure.RoleCounts[representation.RoleTest])
	}

	if len(profile.Structure.Directories) != 3 {
		t.Fatalf("expected 3 directories, got %d: %+v", len(profile.Structure.Directories), profile.Structure.Directories)
	}

	var foundRoot, foundInternal bool
	for _, dir := range profile.Structure.Directories {
		switch dir.Path {
		case "cmd/api":
			if dir.Files != 2 || dir.Chunks != 2 {
				t.Fatalf("cmd/api aggregate wrong: %+v", dir)
			}
		case "internal/pkg":
			if dir.Files != 3 {
				t.Fatalf("internal/pkg aggregate wrong: %+v", dir)
			}
			foundInternal = true
		case ".":
			foundRoot = true
		}
	}
	if !foundRoot || !foundInternal {
		t.Fatalf("missing expected directories: %+v", profile.Structure.Directories)
	}

	if len(profile.Structure.ConfigFiles) != 1 || profile.Structure.ConfigFiles[0].Path != "openapi.yaml" {
		t.Fatalf("config files wrong: %+v", profile.Structure.ConfigFiles)
	}
	if len(profile.Structure.APIContracts) != 1 || profile.Structure.APIContracts[0].Path != "openapi.yaml" {
		t.Fatalf("api contracts wrong: %+v", profile.Structure.APIContracts)
	}
	if len(profile.Structure.TestFiles) != 2 {
		t.Fatalf("test files wrong: %+v", profile.Structure.TestFiles)
	}
}

func TestDiscoverGoEntryPointCommandAndModule(t *testing.T) {
	chunks := []representation.Representation{
		sourceRep("cmd/worker/main.go", "Go", [][2]string{{"main", "package"}}, "package main"),
		sourceRep("cmd/web/main.go", "Go", [][2]string{{"main", "function"}}, "package main"),
		rep("go.mod", "Go", representation.RoleDependencyManifest, nil, "module example.org/x", 0),
	}

	profile := Discover(chunks, DefaultLimits())

	if len(profile.Structure.EntryPoints) != 2 {
		t.Fatalf("expected 2 entry points, got %+v", profile.Structure.EntryPoints)
	}
	if profile.Structure.EntryPoints[0].Path != "cmd/web/main.go" || profile.Structure.EntryPoints[0].Kind != "command" {
		t.Fatalf("first entry wrong: %+v", profile.Structure.EntryPoints[0])
	}
	if profile.Structure.EntryPoints[1].Path != "cmd/worker/main.go" {
		t.Fatalf("second entry wrong: %+v", profile.Structure.EntryPoints[1])
	}

	var cmdWorker, rootModule *AppScope
	for _, scope := range profile.Structure.AppScopes {
		switch {
		case scope.Path == "cmd/worker":
			cmdWorker = &scope
		case scope.Kind == "go_module" && scope.Path == ".":
			rootModule = &scope
		}
	}
	if cmdWorker == nil || cmdWorker.Kind != "command" {
		t.Fatalf("expected cmd/worker command scope, got %+v", profile.Structure.AppScopes)
	}
	if rootModule == nil || len(rootModule.Evidence) != 1 || rootModule.Evidence[0] != "go.mod" {
		t.Fatalf("expected root go_module scope, got %+v", profile.Structure.AppScopes)
	}
}

func TestDiscoverPythonAndWebEntryPoints(t *testing.T) {
	chunks := []representation.Representation{
		sourceRep("app.py", "Python", nil, "if __name__ == \"__main__\":\n    run()"),
		sourceRep("src/server.ts", "TypeScript", [][2]string{{"server", "export"}}, "export const server = 1"),
		sourceRep("lib/utils.ts", "TypeScript", [][2]string{{"utils", "export"}}, "export const utils = 1"),
	}

	profile := Discover(chunks, DefaultLimits())

	var foundPython, foundServer bool
	for _, entry := range profile.Structure.EntryPoints {
		switch entry.Path {
		case "app.py":
			if entry.Kind != "script" || entry.Source != "python_main_guard" {
				t.Fatalf("python entry wrong: %+v", entry)
			}
			foundPython = true
		case "src/server.ts":
			if entry.Kind != "application" {
				t.Fatalf("ts entry kind wrong: %+v", entry)
			}
			foundServer = true
		}
	}
	if !foundPython || !foundServer {
		t.Fatalf("expected python and ts entries, got %+v", profile.Structure.EntryPoints)
	}
}

func TestDiscoverServiceAndPackageScopes(t *testing.T) {
	chunks := []representation.Representation{
		sourceRep("services/billing/svc.go", "Go", nil, "package billing"),
		sourceRep("services/billing/svc_test.go", "Go", nil, "package billing"),
		sourceRep("services/payments/svc.go", "Go", nil, "package payments"),
		sourceRep("pkg/cfg/cfg.go", "Go", nil, "package cfg"),
	}

	profile := Discover(chunks, DefaultLimits())

	var billing, pkg *AppScope
	for _, scope := range profile.Structure.AppScopes {
		switch scope.Path {
		case "services/billing":
			billing = &scope
		case "pkg":
			pkg = &scope
		}
	}
	if billing == nil || billing.Kind != "service" {
		t.Fatalf("expected services/billing service scope, got %+v", profile.Structure.AppScopes)
	}
	if pkg == nil || pkg.Kind != "package" {
		t.Fatalf("expected pkg package scope, got %+v", profile.Structure.AppScopes)
	}
}

func TestConventionsRequireMultipleExamples(t *testing.T) {
	single := Discover([]representation.Representation{
		sourceRep("services/foo/test_helpers.py", "Python", nil, "def helper(): pass"),
	}, DefaultLimits())
	if len(single.Conventions) != 0 {
		t.Fatalf("expected no conventions for a single observation, got %+v", single.Conventions)
	}

	multiple := Discover([]representation.Representation{
		sourceRep("services/a/test_a.py", "Python", nil, "def test_a(): pass"),
		sourceRep("services/b/test_b.py", "Python", nil, "def test_b(): pass"),
		sourceRep("pkg/x_test.go", "Go", nil, "package x"),
		sourceRep("pkg/y_test.go", "Go", nil, "package y"),
	}, DefaultLimits())

	if len(multiple.Conventions) != 2 {
		t.Fatalf("expected 2 conventions, got %+v", multiple.Conventions)
	}
	for _, convention := range multiple.Conventions {
		if convention.Level != LevelDerived {
			t.Fatalf("convention must be derived: %+v", convention)
		}
		if len(convention.Evidence) < 2 {
			t.Fatalf("convention needs >=2 evidence refs: %+v", convention)
		}
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	chunks := []representation.Representation{
		sourceRep("cmd/api/main.go", "Go", [][2]string{{"main", "package"}}, "package main"),
		sourceRep("pkg/x_test.go", "Go", nil, "package x"),
		sourceRep("pkg/y_test.go", "Go", nil, "package y"),
	}

	first := Discover(chunks, DefaultLimits())
	second := Discover(chunks, DefaultLimits())

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("discover must be deterministic:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestDiscoverRespectsLimits(t *testing.T) {
	var chunks []representation.Representation
	for i := 0; i < 80; i++ {
		filePath := "dir" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + "/main.go"
		chunks = append(chunks, sourceRep(filePath, "Go", [][2]string{{"main", "package"}}, "package main"))
	}

	limits := DefaultLimits()
	limits.MaxDirectories = 3
	limits.MaxEntryPoints = 4
	limits.MaxFileRefs = 10

	profile := Discover(chunks, limits)

	if len(profile.Structure.Directories) != 3 {
		t.Fatalf("expected 3 capped directories, got %d", len(profile.Structure.Directories))
	}
	if len(profile.Structure.EntryPoints) != 4 {
		t.Fatalf("expected 4 capped entry points, got %d", len(profile.Structure.EntryPoints))
	}
}

func TestApplyArchitectureLabelsInference(t *testing.T) {
	profile := &Profile{}
	profile.ApplyArchitecture(Architecture{
		Overview: "A Go service",
		Components: []Component{
			{Name: "api", Kind: "service", Description: "HTTP layer"},
		},
		Relationships: []Relationship{
			{From: "api", To: "db", Kind: "calls"},
		},
		EngineeringDecisions: []Statement{
			{Text: "Text: go modules"},
		},
		Uncertainty: []string{"entry points may be incomplete"},
	})

	if profile.Architecture.Overview != "A Go service" {
		t.Fatalf("overview wrong: %+v", profile.Architecture.Overview)
	}
	if len(profile.Architecture.Components) != 1 || profile.Architecture.Components[0].Level != LevelInferred {
		t.Fatalf("component must be inferred: %+v", profile.Architecture.Components)
	}
	if len(profile.Architecture.Relationships) != 1 || profile.Architecture.Relationships[0].Level != LevelInferred {
		t.Fatalf("relationship must be inferred: %+v", profile.Architecture.Relationships)
	}
	if len(profile.Architecture.Uncertainty) != 1 {
		t.Fatalf("uncertainty wrong: %+v", profile.Architecture.Uncertainty)
	}
}
