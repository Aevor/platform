// Package impact implements Repository Impact Analysis: a deterministic,
// repository-scoped dependency and impact graph derived exclusively from Task
// 3f representations (imports, calls, references, implicit interfaces, and
// test→source associations).
//
// Architectural invariants:
//
//   - PURE over representation results: ZERO filesystem, database, or network
//     access. Only authorized repository data (already represented upstream)
//     can enter the graph.
//   - DETERMINISTIC: the same representations always yield byte-identical
//     graphs, edges, and traversal results. Edges are keyed and sorted; no
//     timestamps, no randomness, no map-iteration order leaks.
//   - HONEST CERTAINTY: every edge is labeled `direct` (derived from repository
//     facts such as a literal import or identifier reference) or `inferred`
//     (a conservative structural heuristic such as implicit Go interfaces).
//     The traversal NEVER upgrades an inferred relationship to direct.
//   - BOUNDED: retained graphs and per-analysis results are capped. Truncation
//     is reported, never silent.
package impact

import (
	"sort"
	"strings"
	"sync"

	"github.com/Aevor/platform/services/api/internal/representation"
)

// Edge certainty labels. Repository-derived facts are `direct`; heuristic or
// AI-originated relationships are `inferred`.
const (
	CertaintyDirect   = "direct"
	CertaintyInferred = "inferred"
)

// Relationship kinds. The vocabulary is small, explicit, and stable: it is
// part of the observable API contract.
const (
	KindImports    = "imports"
	KindCalls      = "calls"
	KindReferences = "references"
	KindImplements = "implements"
	KindTests      = "tests"
)

// Build/analysis bounds. No unbounded default is permitted.
const (
	DefaultMaxRepositories = 8
	DefaultMaxEdgesTotal   = 200000

	defaultAnalysisDepth      = 2
	maxAnalysisDepth          = 5
	defaultMaxResults         = 256
	maxResultsCeiling         = 1024
	maxImportTargetsPerImport = 64
	maxTokensPerFile          = 4000
	minReferenceSymbolLength  = 3
)

// Target identifies the repository element a proposed change touches. Symbol
// is optional: a file-level target considers every symbol and relationship of
// the file.
type Target struct {
	FilePath string `json:"file_path"`
	Symbol   string `json:"symbol,omitempty"`
}

// Edge is a directional repository relationship. For dependency edges
// (imports/calls/references/tests) the direction is "From depends on To", so
// impact of a target flows along INBOUND edges: everything pointing at the
// target may be affected by changing it.
type Edge struct {
	FromFile   string `json:"from_file"`
	FromSymbol string `json:"from_symbol,omitempty"`
	ToFile     string `json:"to_file"`
	ToSymbol   string `json:"to_symbol,omitempty"`
	Kind       string `json:"kind"`
	Certainty  string `json:"certainty"`
	Evidence   string `json:"evidence"`
}

type symbolDef struct {
	FilePath   string
	Name       string
	SymbolType string
	Parent     string
}

type interfaceDef struct {
	FilePath string
	Name     string
	Methods  []string
}

// Graph is an immutable, repository-scoped impact graph. It is safe for
// concurrent readers once published by a Store.
type Graph struct {
	repositoryID string

	files        map[string]struct{}
	fileLanguage map[string]string
	fileRole     map[string]string
	fileContent  map[string]string

	defsByName    map[string][]symbolDef
	symbolsByFile map[string][]symbolDef
	methodsByType map[string]map[string]bool
	interfaces    []interfaceDef

	outEdges map[string][]Edge
	inEdges  map[string][]Edge

	truncated bool
}

// RepositoryID returns the repository identity the graph was built for.
func (g *Graph) RepositoryID() string { return g.repositoryID }

// Files returns the represented repository files in lexical order.
func (g *Graph) Files() []string {
	files := make([]string, 0, len(g.files))
	for file := range g.files {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

// Truncated reports whether the builder dropped relationships to stay within
// its bounds.
func (g *Graph) Truncated() bool { return g.truncated }

// Options bounds retained graph memory. Zero values use safe defaults.
type Options struct {
	MaxRepositories int
	MaxEdgesTotal   int
}

func (o Options) maxRepositories() int {
	if o.MaxRepositories > 0 {
		return o.MaxRepositories
	}
	return DefaultMaxRepositories
}

func (o Options) maxEdgesTotal() int {
	if o.MaxEdgesTotal > 0 {
		return o.MaxEdgesTotal
	}
	return DefaultMaxEdgesTotal
}

// Store holds bounded repository-scoped impact graphs. Replace is atomic:
// readers observe either the previous complete graph or the next complete
// graph, never an intermediate mix. Missing graphs never reveal whether a
// repository exists.
type Store struct {
	mu      sync.RWMutex
	options Options
	graphs  map[string]*Graph
}

// NewStore creates an empty, bounded graph store.
func NewStore(options Options) *Store {
	return &Store{options: options, graphs: make(map[string]*Graph)}
}

// Replace atomically installs one repository's complete graph.
func (s *Store) Replace(repositoryID string, graph *Graph) error {
	if strings.TrimSpace(repositoryID) == "" || strings.ContainsAny(repositoryID, "\x00\r\n") {
		return errInvalidRepositoryID
	}
	if graph == nil {
		return errInvalidGraph
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.graphs[repositoryID]; !exists && len(s.graphs) >= s.options.maxRepositories() {
		return errRepositoryLimit
	}

	total := graph.edgeCount()
	if existing := s.graphs[repositoryID]; existing != nil {
		total += s.edgeCountLocked() - existing.edgeCount()
	} else {
		total += s.edgeCountLocked()
	}
	if total > s.options.maxEdgesTotal() {
		return errEdgeLimit
	}

	s.graphs[repositoryID] = graph
	return nil
}

// Remove deletes one repository graph. It is idempotent.
func (s *Store) Remove(repositoryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.graphs, repositoryID)
}

// Get returns one repository graph and whether it is present.
func (s *Store) Get(repositoryID string) (*Graph, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	graph, ok := s.graphs[repositoryID]
	return graph, ok
}

func (s *Store) edgeCountLocked() int {
	total := 0
	for _, graph := range s.graphs {
		total += graph.edgeCount()
	}
	return total
}

func (g *Graph) edgeCount() int {
	total := 0
	for _, edges := range g.outEdges {
		total += len(edges)
	}
	return total
}

// ---------------------------------------------------------------------------
// Build
// ---------------------------------------------------------------------------

type fileChunk struct {
	index   int
	content string
	symType string
	symName string
	parent  string
}

// Build constructs a repository graph from representations. representations
// must all belong to repositoryID; foreign entries are ignored defensively
// (the caller's index/ownership layer is the authority, this package adds no
// trust). The result is never nil.
func Build(repositoryID string, representations []representation.Representation) *Graph {
	graph := &Graph{
		repositoryID:  repositoryID,
		files:         make(map[string]struct{}),
		fileLanguage:  make(map[string]string),
		fileRole:      make(map[string]string),
		fileContent:   make(map[string]string),
		defsByName:    make(map[string][]symbolDef),
		symbolsByFile: make(map[string][]symbolDef),
		methodsByType: make(map[string]map[string]bool),
		outEdges:      make(map[string][]Edge),
		inEdges:       make(map[string][]Edge),
	}

	chunksByFile := make(map[string][]fileChunk)
	testTargets := make(map[string]string)

	for i := range representations {
		item := &representations[i]
		if item.RepositoryID != repositoryID {
			continue
		}
		if item.FilePath == "" {
			continue
		}

		graph.files[item.FilePath] = struct{}{}
		if _, ok := graph.fileLanguage[item.FilePath]; !ok {
			graph.fileLanguage[item.FilePath] = item.Language
		}
		if _, ok := graph.fileRole[item.FilePath]; !ok {
			graph.fileRole[item.FilePath] = item.FileRole
		}
		if item.SourceUnderTest != "" {
			testTargets[item.FilePath] = item.SourceUnderTest
		}

		chunksByFile[item.FilePath] = append(chunksByFile[item.FilePath], fileChunk{
			index:   item.ChunkIndex,
			content: item.Content,
			symType: item.SymbolType,
			symName: deref(item.SymbolName),
			parent:  deref(item.ParentSymbol),
		})
	}

	files := make([]string, 0, len(chunksByFile))
	for file := range chunksByFile {
		files = append(files, file)
	}
	sort.Strings(files)

	edges := make(map[string]Edge)
	edgeOrder := make([]string, 0)

	addEdge := func(edge Edge) {
		if edge.FromFile == "" || edge.ToFile == "" || edge.FromFile == edge.ToFile {
			return
		}
		if edge.Certainty == "" {
			edge.Certainty = CertaintyDirect
		}
		key := edge.FromFile + "\x00" + edge.FromSymbol + "\x00" + edge.ToFile + "\x00" + edge.ToSymbol + "\x00" + edge.Kind
		if _, exists := edges[key]; exists {
			return
		}
		edges[key] = edge
		edgeOrder = append(edgeOrder, key)
	}

	dirToFiles := make(map[string][]string)

	for _, file := range files {
		chunks := chunksByFile[file]
		sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].index < chunks[j].index })

		var content strings.Builder
		for _, chunk := range chunks {
			content.WriteString(chunk.content)
			if !strings.HasSuffix(chunk.content, "\n") {
				content.WriteString("\n")
			}

			if chunk.symType == "imports" {
				continue
			}
			def := symbolDef{FilePath: file, Name: chunk.symName, SymbolType: chunk.symType, Parent: chunk.parent}
			if def.Name != "" {
				graph.defsByName[def.Name] = append(graph.defsByName[def.Name], def)
				graph.symbolsByFile[file] = append(graph.symbolsByFile[file], def)
			}
			if chunk.symType == "method" && chunk.parent != "" && chunk.symName != "" {
				if graph.methodsByType[chunk.parent] == nil {
					graph.methodsByType[chunk.parent] = make(map[string]bool)
				}
				graph.methodsByType[chunk.parent][chunk.symName] = true
			}
			if (chunk.symType == "type_declaration" || chunk.symType == "interface") &&
				strings.Contains(chunk.content, "interface") {
				graph.interfaces = append(graph.interfaces, interfaceDef{
					FilePath: file,
					Name:     chunk.symName,
					Methods:  interfaceMethodNames(chunk.content),
				})
			}
		}
		graph.fileContent[file] = content.String()
		dirToFiles[dirOf(file)] = append(dirToFiles[dirOf(file)], file)
	}

	// 1. Imports.
	for _, file := range files {
		language := graph.fileLanguage[file]
		imports := extractImportPaths(language, graph.importSource(file, chunksByFile[file]))
		for _, raw := range imports {
			targets := resolveImport(file, raw, language, dirToFiles, graph.files)
			for _, targetFile := range targets {
				addEdge(Edge{
					FromFile:  file,
					ToFile:    targetFile,
					Kind:      KindImports,
					Certainty: CertaintyDirect,
					Evidence:  "imports " + raw,
				})
			}
		}
	}

	// 2. Calls and lexical references over repo-defined symbols.
	for _, file := range files {
		tokens := identifiersIn(graph.fileContent[file])
		if len(tokens) == 0 {
			continue
		}
		considered := 0
		names := make([]string, 0, len(tokens))
		for name := range tokens {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			if len(name) < minReferenceSymbolLength {
				continue
			}
			defs := graph.defsByName[name]
			if len(defs) == 0 {
				continue
			}
			if considered >= maxTokensPerFile {
				graph.truncated = true
				break
			}
			considered++

			called := tokens[name]
			kind := KindReferences
			evidence := "identifier " + name + " referenced"
			if called {
				kind = KindCalls
				evidence = name + "(...) called"
			}

			seenTargets := make(map[string]bool)
			for _, def := range defs {
				if def.FilePath == file || seenTargets[def.FilePath] {
					continue
				}
				seenTargets[def.FilePath] = true
				addEdge(Edge{
					FromFile:   file,
					FromSymbol: name,
					ToFile:     def.FilePath,
					ToSymbol:   def.Name,
					Kind:       kind,
					Certainty:  CertaintyDirect,
					Evidence:   evidence,
				})
			}
		}
	}

	// 3. Test → source associations (deterministic, from representation).
	for _, file := range files {
		target, ok := testTargets[file]
		if !ok || target == "" {
			continue
		}
		if _, exists := graph.files[target]; !exists {
			continue
		}
		addEdge(Edge{
			FromFile:  file,
			ToFile:    target,
			Kind:      KindTests,
			Certainty: CertaintyDirect,
			Evidence:  "test covers " + target,
		})
	}

	// 4. Implicit interface implementations (Go-style structural heuristic).
	typeDefs := make([]symbolDef, 0)
	for _, file := range files {
		for _, def := range graph.symbolsByFile[file] {
			if def.SymbolType == "type_declaration" || def.SymbolType == "class" || def.SymbolType == "record" {
				typeDefs = append(typeDefs, def)
			}
		}
	}
	sort.SliceStable(typeDefs, func(i, j int) bool {
		if typeDefs[i].FilePath != typeDefs[j].FilePath {
			return typeDefs[i].FilePath < typeDefs[j].FilePath
		}
		return typeDefs[i].Name < typeDefs[j].Name
	})

	for _, iface := range graph.interfaces {
		if iface.Name == "" || len(iface.Methods) == 0 {
			continue
		}
		for _, candidate := range typeDefs {
			if candidate.FilePath == iface.FilePath || candidate.Name == iface.Name {
				continue
			}
			methods := graph.methodsByType[candidate.Name]
			if !coversAll(methods, iface.Methods) {
				continue
			}
			addEdge(Edge{
				FromFile:   candidate.FilePath,
				FromSymbol: candidate.Name,
				ToFile:     iface.FilePath,
				ToSymbol:   iface.Name,
				Kind:       KindImplements,
				Certainty:  CertaintyInferred,
				Evidence:   "type " + candidate.Name + " implements interface " + iface.Name + " (methods: " + strings.Join(iface.Methods, ", ") + ")",
			})
		}
	}

	// Finalize adjacency in deterministic order.
	for _, key := range edgeOrder {
		edge := edges[key]
		graph.outEdges[edge.FromFile] = append(graph.outEdges[edge.FromFile], edge)
		graph.inEdges[edge.ToFile] = append(graph.inEdges[edge.ToFile], edge)
	}
	for file := range graph.outEdges {
		sortEdges(graph.outEdges[file])
	}
	for file := range graph.inEdges {
		sortEdges(graph.inEdges[file])
	}

	return graph
}

// importSource returns the import-relevant text for a file: the concatenation
// of its imports chunks when present, otherwise the whole content so files too
// small to be structurally segmented still yield their imports.
func (g *Graph) importSource(file string, chunks []fileChunk) string {
	var imports strings.Builder
	found := false
	for _, chunk := range chunks {
		if chunk.symType == "imports" {
			imports.WriteString(chunk.content)
			imports.WriteString("\n")
			found = true
		}
	}
	if found {
		return imports.String()
	}
	return g.fileContent[file]
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func dirOf(file string) string {
	index := strings.LastIndex(file, "/")
	if index < 0 {
		return "."
	}
	return file[:index]
}

func sortEdges(edges []Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		left, right := edges[i], edges[j]
		switch {
		case left.FromFile != right.FromFile:
			return left.FromFile < right.FromFile
		case left.FromSymbol != right.FromSymbol:
			return left.FromSymbol < right.FromSymbol
		case left.ToFile != right.ToFile:
			return left.ToFile < right.ToFile
		case left.ToSymbol != right.ToSymbol:
			return left.ToSymbol < right.ToSymbol
		default:
			return left.Kind < right.Kind
		}
	})
}

func coversAll(methods map[string]bool, required []string) bool {
	if len(methods) == 0 || len(required) == 0 {
		return false
	}
	for _, name := range required {
		if !methods[name] {
			return false
		}
	}
	return true
}
