package impact

import (
	"sort"
	"strings"

	"github.com/Aevor/platform/services/api/internal/representation"
)

// AnalyzeOptions bounds one impact traversal. Zero values use safe defaults;
// Depth is clamped to [1, maxAnalysisDepth] and MaxResults to
// [1, maxResultsCeiling] so no caller can request an unbounded walk.
type AnalyzeOptions struct {
	Depth      int
	MaxResults int
}

func (o AnalyzeOptions) depth() int {
	if o.Depth <= 0 {
		return defaultAnalysisDepth
	}
	if o.Depth > maxAnalysisDepth {
		return maxAnalysisDepth
	}
	return o.Depth
}

func (o AnalyzeOptions) maxResults() int {
	if o.MaxResults <= 0 {
		return defaultMaxResults
	}
	if o.MaxResults > maxResultsCeiling {
		return maxResultsCeiling
	}
	return o.MaxResults
}

// Item is one impacted repository element. Distance is 0 for a direct
// relationship and increases along the dependency traversal. Certainty is
// taken verbatim from the originating edge and is NEVER upgraded.
type Item struct {
	FilePath  string `json:"file_path"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind"`
	Certainty string `json:"certainty"`
	Evidence  string `json:"evidence"`
	Distance  int    `json:"distance"`
}

// Analysis is the complete, deterministic impact view for one target. Direct
// impacts come straight from repository-derived (or clearly inferred) edges;
// possible impacts are the bounded transitive closure. AI-only findings are
// attached separately by the service layer and never mixed in silently.
type Analysis struct {
	RepositoryID  string   `json:"repository_id"`
	Target        Target   `json:"target"`
	Direct        []Item   `json:"direct_impacts"`
	Indirect      []Item   `json:"possible_impacts"`
	Tests         []Item   `json:"related_tests"`
	Configuration []Item   `json:"related_configuration"`
	APISurface    []Item   `json:"related_api_surface"`
	Risks         []string `json:"risks"`
	Unknowns      []string `json:"unknowns"`
	Truncated     bool     `json:"truncated"`
	Depth         int      `json:"depth"`
	AnalyzedFiles int      `json:"analyzed_files"`
	Unsupported   bool     `json:"unsupported_language"`
}

// Analyze computes the impact of changing target against this graph. It never
// fails and never invents relationships: an unknown or unsupported target
// yields an empty result plus explicit Unknowns, not an error.
func (g *Graph) Analyze(target Target, options AnalyzeOptions) *Analysis {
	depth := options.depth()
	maxResults := options.maxResults()

	analysis := &Analysis{
		RepositoryID:  g.repositoryID,
		Target:        target,
		Direct:        []Item{},
		Indirect:      []Item{},
		Tests:         []Item{},
		Configuration: []Item{},
		APISurface:    []Item{},
		Risks:         []string{},
		Unknowns:      []string{},
		Depth:         depth,
		AnalyzedFiles: len(g.files),
	}

	if strings.TrimSpace(target.FilePath) == "" {
		analysis.Unknowns = append(analysis.Unknowns, "target file path is required")
		return analysis
	}

	if _, ok := g.files[target.FilePath]; !ok {
		analysis.Unknowns = append(analysis.Unknowns,
			"target file is not part of the current repository index")
		return analysis
	}

	if target.Symbol != "" {
		found := false
		for _, def := range g.defsByName[target.Symbol] {
			if def.FilePath == target.FilePath {
				found = true
				break
			}
		}
		if !found {
			analysis.Unknowns = append(analysis.Unknowns,
				"symbol "+target.Symbol+" is not defined in "+target.FilePath)
		}
	}

	language := g.fileLanguage[target.FilePath]
	switch language {
	case "Go", "Python", "JavaScript", "TypeScript":
	default:
		analysis.Unsupported = true
		label := language
		if label == "" {
			label = "unknown"
		}
		analysis.Unknowns = append(analysis.Unknowns,
			"language "+label+" has no structural relationship extraction")
	}

	// Direct impacts: inbound dependents plus the target's own dependencies.
	direct := make(map[string]Item)
	directOrder := make([]string, 0)

	addDirect := func(item Item) {
		item.Distance = 0
		key := item.FilePath + "\x00" + item.Symbol + "\x00" + item.Kind
		if _, exists := direct[key]; exists {
			return
		}
		direct[key] = item
		directOrder = append(directOrder, key)
	}

	for _, edge := range g.inEdges[target.FilePath] {
		if !edgeMatchesTarget(edge, target.Symbol) {
			continue
		}
		addDirect(Item{
			FilePath:  edge.FromFile,
			Symbol:    edge.FromSymbol,
			Kind:      edge.Kind,
			Certainty: edge.Certainty,
			Evidence:  edge.Evidence,
		})
	}

	for _, edge := range g.outEdges[target.FilePath] {
		addDirect(Item{
			FilePath:  edge.ToFile,
			Symbol:    edge.ToSymbol,
			Kind:      edge.Kind,
			Certainty: edge.Certainty,
			Evidence:  edge.Evidence,
		})
	}

	sort.SliceStable(directOrder, func(i, j int) bool {
		return lessItem(direct[directOrder[i]], direct[directOrder[j]])
	})

	// Transitive, inbound-only expansion: what depends on what depends on the
	// target. Bounded by depth and by maxResults.
	visited := map[string]bool{target.FilePath: true}
	frontier := make([]string, 0, len(direct))
	for _, key := range directOrder {
		item := direct[key]
		analysis.Direct = append(analysis.Direct, item)
		if !visited[item.FilePath] {
			visited[item.FilePath] = true
			frontier = append(frontier, item.FilePath)
		}
	}

	indirect := make(map[string]Item)
	indirectOrder := make([]string, 0)

	budget := maxResults - len(analysis.Direct)
	if budget < 0 {
		budget = 0
	}
	truncated := false

	for distance := 1; distance <= depth && len(frontier) > 0; distance++ {
		next := make([]string, 0)

		for _, file := range frontier {
			for _, edge := range g.inEdges[file] {
				if visited[edge.FromFile] {
					continue
				}
				if budget == 0 {
					truncated = true
					break
				}
				visited[edge.FromFile] = true
				budget--

				item := Item{
					FilePath:  edge.FromFile,
					Symbol:    edge.FromSymbol,
					Kind:      edge.Kind,
					Certainty: edge.Certainty,
					Evidence:  edge.Evidence,
					Distance:  distance,
				}
				key := item.FilePath + "\x00" + item.Symbol + "\x00" + item.Kind
				if _, exists := indirect[key]; !exists {
					indirect[key] = item
					indirectOrder = append(indirectOrder, key)
				}
				next = append(next, edge.FromFile)
			}
			if truncated {
				break
			}
		}
		if truncated {
			break
		}
		frontier = next
	}

	sort.SliceStable(indirectOrder, func(i, j int) bool {
		return lessItem(indirect[indirectOrder[i]], indirect[indirectOrder[j]])
	})
	for _, key := range indirectOrder {
		analysis.Indirect = append(analysis.Indirect, indirect[key])
	}
	analysis.Truncated = truncated || g.truncated

	// Classify affected files into the review categories.
	seenItem := make(map[string]bool)
	classify := func(item Item) {
		key := item.FilePath + "\x00" + item.Symbol + "\x00" + item.Kind
		if seenItem[key] {
			return
		}
		seenItem[key] = true

		role := g.fileRole[item.FilePath]
		switch {
		case item.Kind == KindTests || role == representation.RoleTest:
			analysis.Tests = append(analysis.Tests, item)
		case role == representation.RoleConfiguration || role == representation.RoleDependencyManifest ||
			role == representation.RoleBuild || role == representation.RoleCI:
			analysis.Configuration = append(analysis.Configuration, item)
		case apiSurface(item.FilePath):
			analysis.APISurface = append(analysis.APISurface, item)
		}
	}
	for _, item := range analysis.Direct {
		classify(item)
	}
	for _, item := range analysis.Indirect {
		classify(item)
	}

	// Risks and unknowns are deterministic observations, never scores.
	inferredSeen := false
	for _, item := range analysis.Direct {
		if item.Certainty == CertaintyInferred {
			inferredSeen = true
			break
		}
	}
	for _, item := range analysis.Indirect {
		if item.Certainty == CertaintyInferred {
			inferredSeen = true
			break
		}
	}

	if len(analysis.Direct) == 0 {
		analysis.Risks = append(analysis.Risks,
			"no repository-derived relationships found for this target; impact may be limited or the target may be isolated")
	}
	if inferredSeen {
		analysis.Risks = append(analysis.Risks,
			"one or more relationships are structural inferences (inferred) and require human review")
	}
	if analysis.Truncated {
		analysis.Risks = append(analysis.Risks,
			"impact traversal was truncated at the configured limit; additional relationships may exist")
	}
	if analysis.Unsupported {
		analysis.Risks = append(analysis.Risks,
			"the target language is not structurally analyzed; only lexical and test relationships are reported")
	}

	return analysis
}

// edgeMatchesTarget decides whether an inbound edge pertains to the requested
// symbol. File-level edges (no ToSymbol) always apply; symbol edges apply only
// to the requested symbol. A file-level target matches every edge.
func edgeMatchesTarget(edge Edge, symbol string) bool {
	if symbol == "" {
		return true
	}
	return edge.ToSymbol == "" || edge.ToSymbol == symbol
}

func lessItem(left, right Item) bool {
	if left.Distance != right.Distance {
		return left.Distance < right.Distance
	}
	if left.FilePath != right.FilePath {
		return left.FilePath < right.FilePath
	}
	if left.Symbol != right.Symbol {
		return left.Symbol < right.Symbol
	}
	return left.Kind < right.Kind
}

func apiSurface(file string) bool {
	base := strings.ToLower(pathBase(file))
	if strings.HasSuffix(base, ".proto") || strings.HasSuffix(base, ".graphql") {
		return true
	}
	for _, marker := range []string{"handler", "route", "router", "controller", "endpoint", "openapi", "swagger", "api"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return false
}

func pathBase(file string) string {
	if index := strings.LastIndex(file, "/"); index >= 0 {
		return file[index+1:]
	}
	return file
}
