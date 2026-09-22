// Package repoinfo produces the deterministic, machine-readable repository
// intelligence profile for one Aevor repository.
//
// It is PURE: the input is a slice of representation records (Task 3f output)
// plus optional AI-provided architecture notes. There is ZERO filesystem
// access, ZERO database access, and ZERO further model inference here — every
// security property (ownership, path safety, bounded content) is inherited
// from the bounded representation pipeline by construction.
//
// Provenance model — never blur the lines:
//
//   - LevelDeterministic: facts derived directly from indexed representation
//     metadata (languages, roles, files, config, tests, build markers).
//   - LevelDerived: clearly labeled information computed deterministically
//     from deterministic facts (entry points, app scopes, conventions, API
//     contracts). Each item carries its evidence (repository-relative paths).
//   - LevelInferred: AI-labeled notes (architecture overview, relationships,
//     conventions, uncertainty). Never overrides deterministic facts.
//
// Repository content is untrusted data here: a handful of well-known literal
// markers (e.g. Python's __main__ guard) may be scanned in bounded content to
// detect entry points, but no content ever leaves this package and nothing is
// treated as instructions.
package repoinfo

import (
	"path"
	"sort"
	"strings"

	"github.com/Aevor/platform/services/api/internal/representation"
)

// Level is the provenance level of one repository-intelligence item.
type Level string

const (
	// LevelDeterministic marks facts derived directly from indexed metadata.
	LevelDeterministic Level = "deterministic"
	// LevelDerived marks deterministic computations over deterministic facts,
	// each carrying concrete evidence references.
	LevelDerived Level = "derived"
	// LevelInferred marks AI-provided interpretation. It is labeled inferred
	// and can never override a deterministic or derived fact.
	LevelInferred Level = "inferred"
)

// Limits bounds the profile so a pathological repository cannot produce an
// unbounded payload. Zero values fall back to DefaultLimits.
type Limits struct {
	MaxDirectories int
	MaxEntryPoints int
	MaxAppScopes   int
	MaxFileRefs    int
	MaxConventions int
}

// DefaultLimits returns the built-in safe profile bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxDirectories: 64,
		MaxEntryPoints: 32,
		MaxAppScopes:   32,
		MaxFileRefs:    256,
		MaxConventions: 32,
	}
}

func (l Limits) maxDirectories() int {
	return firstPositive(l.MaxDirectories, DefaultLimits().MaxDirectories)
}
func (l Limits) maxEntryPoints() int {
	return firstPositive(l.MaxEntryPoints, DefaultLimits().MaxEntryPoints)
}
func (l Limits) maxAppScopes() int {
	return firstPositive(l.MaxAppScopes, DefaultLimits().MaxAppScopes)
}
func (l Limits) maxFileRefs() int { return firstPositive(l.MaxFileRefs, DefaultLimits().MaxFileRefs) }
func (l Limits) maxConventions() int {
	return firstPositive(l.MaxConventions, DefaultLimits().MaxConventions)
}

func firstPositive(value int, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// maxEntryScanBytes bounds the per-file content scanned for entry markers. The
// content itself never leaves this package.
const maxEntryScanBytes = 4096

// Profile is the top-level repository intelligence profile.
type Profile struct {
	Structure    Structure    `json:"structure"`
	Architecture Architecture `json:"architecture,omitempty"`
	Conventions  []Convention `json:"conventions,omitempty"`
}

// Structure holds the deterministic and derived repository facts.
type Structure struct {
	LanguageCounts map[string]int `json:"language_counts"`
	RoleCounts     map[string]int `json:"role_counts"`
	Files          int            `json:"files"`
	Chunks         int            `json:"chunks"`
	Directories    []Directory    `json:"directories"`
	EntryPoints    []EntryPoint   `json:"entry_points,omitempty"`
	AppScopes      []AppScope     `json:"app_scopes,omitempty"`
	ConfigFiles    []FileRef      `json:"config_files,omitempty"`
	TestFiles      []FileRef      `json:"test_files,omitempty"`
	BuildFiles     []FileRef      `json:"build_files,omitempty"`
	APIContracts   []FileRef      `json:"api_contracts,omitempty"`
}

// Directory is one repository directory aggregate. Path is repository-relative
// ("." for the root).
type Directory struct {
	Path      string   `json:"path"`
	Files     int      `json:"files"`
	Chunks    int      `json:"chunks"`
	Languages []string `json:"languages,omitempty"`
}

// EntryPoint is one derived process/entry entry point with its source rule.
type EntryPoint struct {
	Path     string   `json:"path"`
	Symbol   string   `json:"symbol,omitempty"`
	Language string   `json:"language"`
	Kind     string   `json:"kind"` // application | command | script | service
	Source   string   `json:"source"`
	Evidence []string `json:"evidence,omitempty"`
}

// AppScope is one derived app/service/package scope of the repository.
type AppScope struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // app | service | command | package | intra-package | go_module
	Path        string   `json:"path"`
	Languages   []string `json:"languages,omitempty"`
	EntryPoints []string `json:"entry_points,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
}

// FileRef references one repository-relative file with its representation role.
type FileRef struct {
	Path string `json:"path"`
	Role string `json:"role"`
}

// Architecture holds the AI-summarized layer. Unless labeled otherwise every
// item here is LevelInferred and must never override deterministic facts.
type Architecture struct {
	Overview             string         `json:"overview,omitempty"`
	Components           []Component    `json:"components,omitempty"`
	Relationships        []Relationship `json:"relationships,omitempty"`
	EngineeringDecisions []Statement    `json:"engineering_decisions,omitempty"`
	Databases            []Statement    `json:"databases,omitempty"`
	ExternalIntegrations []Statement    `json:"external_integrations,omitempty"`
	Uncertainty          []string       `json:"uncertainty,omitempty"`
}

// Component is one architecture component. AI-derived components carry
// LevelInferred; derived components are marked accordingly.
type Component struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	Path             string   `json:"path,omitempty"`
	Description      string   `json:"description"`
	Responsibilities []string `json:"responsibilities,omitempty"`
	Level            Level    `json:"level"`
	Evidence         []string `json:"evidence,omitempty"`
}

// Relationship is one architecture relationship between repository elements.
type Relationship struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     string   `json:"kind"`
	Level    Level    `json:"level"`
	Evidence []string `json:"evidence,omitempty"`
}

// Statement is one labeled architecture statement (decision, database,
// external integration).
type Statement struct {
	Text     string   `json:"text"`
	Level    Level    `json:"level"`
	Evidence []string `json:"evidence,omitempty"`
}

// Convention is one repository convention with concrete evidence references.
// Conventions require multiple supporting examples (>=2) with evidence refs;
// single-occurrence observations never reach this list.
type Convention struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Level       Level    `json:"level"`
	Evidence    []string `json:"evidence,omitempty"`
}

// Discover derives the deterministic structure profile from one repository's
// representations. It never fails: inputs are validated and bounded upstream,
// so undeterminable content becomes empty/absent rather than an error.
func Discover(chunks []representation.Representation, limits Limits) *Profile {
	files := indexFiles(chunks)
	entryPoints := entryPointsOf(files)

	return &Profile{
		Structure:   buildStructure(files, chunks, limits, entryPoints),
		Conventions: buildConventions(files, limits),
	}
}

// fileInfo aggregates everything Discover derives about one represented file.
type fileInfo struct {
	path        string
	directory   string
	language    string
	role        string
	extension   string
	chunks      int
	contentScan string
	symbols     map[symbolKey]bool
}

type symbolKey struct {
	name string
	typ  string
}

func indexFiles(chunks []representation.Representation) map[string]*fileInfo {
	files := make(map[string]*fileInfo, 64)

	for i := range chunks {
		chunk := &chunks[i]
		file := files[chunk.FilePath]
		if file == nil {
			file = &fileInfo{
				path:      chunk.FilePath,
				directory: directoryOf(chunk.Directory, chunk.FilePath),
				language:  chunk.Language,
				role:      chunk.FileRole,
				extension: chunk.Extension,
				symbols:   make(map[symbolKey]bool),
			}
			files[chunk.FilePath] = file
		}

		file.chunks++

		if chunk.SymbolName != nil && chunk.SymbolType != "" {
			file.symbols[symbolKey{name: *chunk.SymbolName, typ: chunk.SymbolType}] = true
		}

		if len(file.contentScan) < maxEntryScanBytes {
			file.contentScan += chunk.Content
			if len(file.contentScan) > maxEntryScanBytes {
				file.contentScan = file.contentScan[:maxEntryScanBytes]
			}
		}
	}

	return files
}

func directoryOf(directory string, filePath string) string {
	if directory != "" {
		if directory == "." {
			return "."
		}
		return directory
	}
	return path.Dir(filePath)
}

func sortedFiles(files map[string]*fileInfo) []*fileInfo {
	out := make([]*fileInfo, 0, len(files))
	for _, file := range files {
		out = append(out, file)
	}
	return rankFiles(out)
}

// rankFiles sorts file metadata by repository-relative path.
func rankFiles(files []*fileInfo) []*fileInfo {
	out := append([]*fileInfo(nil), files...)
	sort.Slice(out, func(i int, j int) bool { return out[i].path < out[j].path })
	return out
}

func buildStructure(files map[string]*fileInfo, chunks []representation.Representation, limits Limits, entryPoints []EntryPoint) Structure {
	structure := Structure{
		LanguageCounts: make(map[string]int),
		RoleCounts:     make(map[string]int),
		Files:          len(files),
		Chunks:         len(chunks),
	}

	directories := make(map[string]*Directory)
	ordered := sortedFiles(files)

	for _, file := range ordered {
		if file.language != "" {
			structure.LanguageCounts[file.language]++
		}
		structure.RoleCounts[file.role]++

		dir := directories[file.directory]
		if dir == nil {
			dir = &Directory{Path: file.directory}
			directories[file.directory] = dir
		}
		dir.Files++
		dir.Chunks += file.chunks
		dir.Languages = appendUnique(dir.Languages, file.language)
	}

	structure.Directories = capDirectories(sortedDirectories(directories), limits.maxDirectories())
	structure.EntryPoints = capEntryPoints(entryPoints, limits.maxEntryPoints())
	structure.AppScopes = capAppScopes(appScopesOf(ordered, entryPoints), limits.maxAppScopes())

	maxRefs := limits.maxFileRefs()
	structure.ConfigFiles = fileRefs(ordered, maxRefs, func(f *fileInfo) bool { return f.role == representation.RoleConfiguration })
	structure.TestFiles = fileRefs(ordered, maxRefs, func(f *fileInfo) bool { return f.role == representation.RoleTest })
	structure.BuildFiles = fileRefs(ordered, maxRefs, func(f *fileInfo) bool {
		return f.role == representation.RoleBuild ||
			f.role == representation.RoleDependencyManifest ||
			f.role == representation.RoleCI
	})
	structure.APIContracts = fileRefs(ordered, maxRefs, isAPIContract)

	return structure
}

// entryPointsOf derives process/entry entry points with concrete evidence.
// Rules are deterministic and deliberately conservative; unknown conventions
// simply produce no entry point.
func entryPointsOf(files map[string]*fileInfo) []EntryPoint {
	entry := make([]EntryPoint, 0, 8)

	for _, file := range sortedFiles(files) {
		if item, ok := goEntryPoint(file); ok {
			entry = append(entry, item)
			continue
		}
		if item, ok := pythonEntryPoint(file); ok {
			entry = append(entry, item)
			continue
		}
		if item, ok := webEntryPoint(file); ok {
			entry = append(entry, item)
		}
	}

	return entry
}

// goEntryPoint recognizes a Go application/command entry: the file that
// declares package main and/or defines func main. Files under a cmd/ root are
// commands regardless of which symbol bracket they reveal first.
func goEntryPoint(file *fileInfo) (EntryPoint, bool) {
	if file.language != "Go" {
		return EntryPoint{}, false
	}

	if file.symbols[symbolKey{name: "main", typ: "package"}] {
		return EntryPoint{
			Path:     file.path,
			Symbol:   "package main",
			Language: file.language,
			Kind:     goEntryKind(file),
			Source:   "go_package_main",
			Evidence: []string{file.path},
		}, true
	}

	if file.symbols[symbolKey{name: "main", typ: "function"}] {
		return EntryPoint{
			Path:     file.path,
			Symbol:   "func main",
			Language: file.language,
			Kind:     goEntryKind(file),
			Source:   "go_func_main",
			Evidence: []string{file.path},
		}, true
	}

	return EntryPoint{}, false
}

func goEntryKind(file *fileInfo) string {
	if file.directory == "cmd" || strings.HasPrefix(file.directory, "cmd/") {
		return "command"
	}
	return "application"
}

// pythonEntryPoint recognizes a Python script/application entry via the
// __main__ guard in bounded scanned content.
func pythonEntryPoint(file *fileInfo) (EntryPoint, bool) {
	if file.language != "Python" {
		return EntryPoint{}, false
	}

	if strings.Contains(file.contentScan, "__main__") {
		return EntryPoint{
			Path:     file.path,
			Symbol:   "__main__",
			Language: file.language,
			Kind:     "script",
			Source:   "python_main_guard",
			Evidence: []string{file.path},
		}, true
	}

	return EntryPoint{}, false
}

// webEntryPoint recognizes conventional JS/TS entry basenames at the root or
// inside a conventional source root.
func webEntryPoint(file *fileInfo) (EntryPoint, bool) {
	if file.language != "JavaScript" && file.language != "TypeScript" {
		return EntryPoint{}, false
	}

	base := strings.ToLower(strings.TrimSuffix(path.Base(file.path), path.Ext(file.path)))
	if base == "" {
		return EntryPoint{}, false
	}

	var supported bool
	switch base {
	case "main", "index", "app", "server":
		supported = true
	}

	if !supported {
		return EntryPoint{}, false
	}

	var convention bool
	switch file.directory {
	case ".", "src", "src/", "lib", "app":
		convention = true
	}

	// A top-level vendor/examples dir must not be treated as the app entry.
	if !convention && strings.Count(file.directory, "/") > 1 {
		return EntryPoint{}, false
	}

	kind := "application"
	if file.role == representation.RoleTest {
		kind = "service"
	}

	return EntryPoint{
		Path:     file.path,
		Symbol:   base,
		Language: file.language,
		Kind:     kind,
		Source:   "web_entry_basename",
		Evidence: []string{file.path},
	}, true
}

// appScopesOf derives app/service/package scopes from conventional roots and
// a root Go module when present. Every scope carries concrete file evidence;
// unconventional layouts simply produce fewer scopes.
//
// Conventional roots: cmd/<cmd> (command), apps/|app/<name> (app),
// services/|service/<name> (service), pkg|lib|src|internal/<...> (package).
// A root go.mod yields a deterministic go_module scope.
func appScopesOf(files []*fileInfo, entryPoints []EntryPoint) []AppScope {
	entryPaths := make(map[string][]string)
	for _, entry := range entryPoints {
		dir := entry.Path
		entryPaths[dir] = append(entryPaths[dir], entry.Path)
	}

	scopes := make([]AppScope, 0, 8)
	seen := make(map[string]bool)

	ordered := rankFiles(files)

	for _, file := range ordered {
		if file.directory == "." {
			continue
		}

		parts := strings.Split(file.directory, "/")
		base := parts[0]

		scopeName := base
		kind := packageKind(base)
		if kind != "" && len(parts) > 1 {
			// cmd/api → name "api"; pkg/foo → stay "pkg" (whole tree is the package root).
			switch base {
			case "cmd", "apps", "app", "services", "service":
				scopeName = parts[1]
			}
		}
		if kind == "" {
			continue
		}

		key := base + "/" + scopeName
		if seen[key] {
			continue
		}
		seen[key] = true

		path := scopePath(base, scopeName)
		var evidence []string
		var languages []string
		var entries []string
		for _, candidate := range ordered {
			if !inScope(candidate.path, path) {
				continue
			}
			if len(evidence) < 3 {
				evidence = append(evidence, candidate.path)
			}
			languages = appendUnique(languages, candidate.language)
			entries = append(entries, entryPaths[candidate.path]...)
		}

		scopes = append(scopes, AppScope{
			Name:        scopeName,
			Kind:        kind,
			Path:        path,
			Languages:   languages,
			EntryPoints: entries,
			Evidence:    evidence,
		})
	}

	if rootModule(files) {
		scopes = append(scopes, AppScope{
			Name:     "root",
			Kind:     "go_module",
			Path:     ".",
			Evidence: []string{"go.mod"},
		})
	}

	return scopes
}

// packageKind maps a top-level directory to an app-scope kind, or "" if the
// directory is not a conventional scope root.
func packageKind(base string) string {
	switch base {
	case "cmd":
		return "command"
	case "apps", "app":
		return "app"
	case "services", "service":
		return "service"
	case "pkg", "lib", "src", "internal":
		return "package"
	default:
		return ""
	}
}

func scopePath(base string, scopeName string) string {
	switch base {
	case "cmd", "apps", "app", "services", "service":
		return base + "/" + scopeName
	default:
		return base
	}
}

func inScope(filePath string, scopePath string) bool {
	if filePath == scopePath {
		return true
	}
	return strings.HasPrefix(filePath, scopePath+"/")
}

// rootModule reports whether the repository root declares a Go module.
func rootModule(files []*fileInfo) bool {
	for _, file := range files {
		if file.directory == "." && strings.EqualFold(path.Base(file.path), "go.mod") {
			return true
		}
	}
	return false
}

func sortedDirectories(directories map[string]*Directory) []Directory {
	out := make([]Directory, 0, len(directories))
	for _, dir := range directories {
		sort.Strings(dir.Languages)
		out = append(out, *dir)
	}
	sort.Slice(out, func(i int, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func appendUnique(items []string, value string) []string {
	if value == "" {
		return items
	}
	for _, existing := range items {
		if existing == value {
			return items
		}
	}
	return append(items, value)
}

func fileRefs(files []*fileInfo, max int, keep func(*fileInfo) bool) []FileRef {
	refs := make([]FileRef, 0, max)
	for _, file := range files {
		if len(refs) >= max {
			break
		}
		if keep(file) {
			refs = append(refs, FileRef{Path: file.path, Role: file.role})
		}
	}
	return refs
}

func isAPIContract(file *fileInfo) bool {
	base := strings.ToLower(path.Base(file.path))
	switch strings.ToLower(file.extension) {
	case ".proto", ".graphql":
		return true
	}
	for _, prefix := range []string{"openapi.", "swagger."} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

func capDirectories(items []Directory, max int) []Directory {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capEntryPoints(items []EntryPoint, max int) []EntryPoint {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capAppScopes(items []AppScope, max int) []AppScope {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capConventions(items []Convention, max int) []Convention {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capStatements(items []Statement, max int) []Statement {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capComponents(items []Component, max int) []Component {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capRelationships(items []Relationship, max int) []Relationship {
	if len(items) < max {
		return items
	}
	return items[:max]
}

func capStrings(items []string, max int) []string {
	if len(items) < max {
		return items
	}
	return items[:max]
}

// MaxArchitectureEntries bounds AI-provided architecture lists so a misbehaving
// service cannot flood a persisted profile.
const MaxArchitectureEntries = 128

// ApplyArchitecture folds validated, AI-inferred architecture into the
// profile. Every incoming item is labeled LevelInferred; supplied Levels are
// overridden. Markers never blend with derived facts. callers pass a bounded
// Architecture (validated upstream against Limits).
func (c *Profile) ApplyArchitecture(architecture Architecture) {
	c.Architecture = Architecture{
		Overview: truncate(architecture.Overview, 2048),
		Components: capComponents(labeledComponents(architecture.Components),
			MaxArchitectureEntries),
		Relationships: capRelationships(labeledRelationships(architecture.Relationships),
			MaxArchitectureEntries),
		EngineeringDecisions: capStatements(labeledStatements(architecture.EngineeringDecisions),
			MaxArchitectureEntries),
		Databases: capStatements(labeledStatements(architecture.Databases),
			MaxArchitectureEntries),
		ExternalIntegrations: capStatements(labeledStatements(architecture.ExternalIntegrations),
			MaxArchitectureEntries),
		Uncertainty: capStrings(labeledStrings(architecture.Uncertainty), MaxArchitectureEntries),
	}
}

func labeledComponents(items []Component) []Component {
	out := make([]Component, 0, len(items))
	for _, item := range items {
		item.Name = truncate(item.Name, 256)
		item.Kind = truncate(item.Kind, 64)
		item.Path = truncate(item.Path, 1024)
		item.Description = truncate(item.Description, 512)
		item.Responsibilities = capStrings(labeledStrings(item.Responsibilities), 32)
		if item.Level == "" {
			item.Level = LevelInferred
		}
		item.Evidence = capStrings(labeledStrings(item.Evidence), 16)
		out = append(out, item)
	}
	return out
}

func labeledRelationships(items []Relationship) []Relationship {
	out := make([]Relationship, 0, len(items))
	for _, item := range items {
		item.From = truncate(item.From, 1024)
		item.To = truncate(item.To, 1024)
		item.Kind = truncate(item.Kind, 64)
		if item.Level == "" {
			item.Level = LevelInferred
		}
		item.Evidence = capStrings(labeledStrings(item.Evidence), 16)
		out = append(out, item)
	}
	return out
}

func labeledStatements(items []Statement) []Statement {
	out := make([]Statement, 0, len(items))
	for _, item := range items {
		item.Text = truncate(item.Text, 512)
		if item.Level == "" {
			item.Level = LevelInferred
		}
		item.Evidence = capStrings(labeledStrings(item.Evidence), 16)
		out = append(out, item)
	}
	return out
}

func labeledStrings(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, truncate(trimmed, 512))
		}
	}
	return out
}

func truncate(value string, max int) string {
	runeCount := 0
	for range value {
		runeCount++
		if runeCount >= max {
			return value
		}
	}
	return value
}

// buildConventions derives repository conventions deterministically. Every
// convention requires at least two supporting examples with evidence refs;
// single-occurrence observations never reach this list.
func buildConventions(files map[string]*fileInfo, limits Limits) []Convention {
	var conventions []Convention

	conventions = append(conventions, testNameConventions(files)...)

	return capConventions(conventions, limits.maxConventions())
}

// testNameConventions recognizes per-language testing naming conventions once
// at least two consistent examples exist.
func testNameConventions(files map[string]*fileInfo) []Convention {
	var goTests []string
	var pythonPrefixTests []string
	var pythonSuffixTests []string
	var webTests []string

	for _, file := range sortedFiles(files) {
		base := strings.ToLower(path.Base(file.path))

		switch {
		case strings.HasSuffix(base, "_test.go"):
			goTests = append(goTests, file.path)
		case strings.HasPrefix(base, "test_"):
			pythonPrefixTests = append(pythonPrefixTests, file.path)
		case strings.HasSuffix(base, "_test.py"):
			pythonSuffixTests = append(pythonSuffixTests, file.path)
		case strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".spec.js"),
			strings.HasSuffix(base, ".test.jsx"), strings.HasSuffix(base, ".spec.jsx"),
			strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".spec.ts"),
			strings.HasSuffix(base, ".test.tsx"), strings.HasSuffix(base, ".spec.tsx"):
			webTests = append(webTests, file.path)
		}
	}

	var conventions []Convention

	if len(goTests) >= 2 {
		conventions = append(conventions, Convention{
			Name:        "Go tests in same-directory _test.go files",
			Description: "Go code is tested with co-located *_test.go files next to the source they cover.",
			Level:       LevelDerived,
			Evidence:    capStrings(goTests, 16),
		})
	}

	if len(pythonPrefixTests) >= 2 {
		conventions = append(conventions, Convention{
			Name:        "Python test_* naming",
			Description: "Python tests use the test_<name>.py prefix convention.",
			Level:       LevelDerived,
			Evidence:    capStrings(pythonPrefixTests, 16),
		})
	}

	if len(pythonSuffixTests) >= 2 {
		conventions = append(conventions, Convention{
			Name:        "Python *_test naming",
			Description: "Python tests use the <name>_test.py suffix convention.",
			Level:       LevelDerived,
			Evidence:    capStrings(pythonSuffixTests, 16),
		})
	}

	if len(webTests) >= 2 {
		conventions = append(conventions, Convention{
			Name:        "JavaScript/TypeScript *.test.* / *.spec.* naming",
			Description: "Frontend/JS tests live next to their source using .test.{js,ts} or .spec.{js,ts} names.",
			Level:       LevelDerived,
			Evidence:    capStrings(webTests, 16),
		})
	}

	return conventions
}
