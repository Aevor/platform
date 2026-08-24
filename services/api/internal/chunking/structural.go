package chunking

import (
	"regexp"
	"strings"
)

// languageRules describes the heuristic structural segmentation for one
// language family. classify recognizes a TOP-LEVEL declaration line (column 0
// keyword start) and returns best-effort symbol metadata; absorbPrefixes are
// line prefixes that attach to a FOLLOWING boundary (doc comments, Java
// annotations). These are deliberate heuristics, NOT an AST framework.
type languageRules struct {
	classify       func(line string) (unitMeta, bool)
	absorbPrefixes []string
}

// unit is one structural segment of a file as half-open line indices
// [start, endExcl).
type unit struct {
	start   int
	endExcl int
	meta    unitMeta
}

type unitMeta struct {
	symbolName   string
	symbolType   string
	parentSymbol string

	// mergeable marks trivial headers (imports/package) whose consecutive
	// lines form ONE unit instead of one unit per line.
	mergeable bool
}

func rulesFor(language string) *languageRules {
	switch language {
	case "Go":
		return &goRules
	case "Python":
		return &pythonRules
	case "JavaScript", "TypeScript":
		return &javaScriptRules
	case "Java":
		return &javaRules
	default:
		return nil
	}
}

var (
	goFuncPattern = regexp.MustCompile(`^func (?:\(([^)]*)\) )?([A-Za-z_][A-Za-z0-9_]*)`)
	goTypePattern = regexp.MustCompile(`^type ([A-Za-z_][A-Za-z0-9_]*)`)
	goDeclPattern = regexp.MustCompile(`^(?:const|var)(?: ([A-Za-z_][A-Za-z0-9_]*))?`)

	pythonFuncPattern  = regexp.MustCompile(`^(?:async )?def ([A-Za-z_][A-Za-z0-9_]*)`)
	pythonClassPattern = regexp.MustCompile(`^class ([A-Za-z_][A-Za-z0-9_]*)`)

	jsFunctionPattern = regexp.MustCompile(`^(?:async )?function \*?([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsClassPattern    = regexp.MustCompile(`^class ([A-Za-z_$][A-Za-z0-9_$]*)`)
	jsExportPattern   = regexp.MustCompile(`^export (?:default )?(?:function \*?([A-Za-z_$][A-Za-z0-9_$]*)|class ([A-Za-z_$][A-Za-z0-9_$]*))?`)

	javaTypePattern = regexp.MustCompile(`^(?:[a-zA-Z@][\w.<>\[\]]*\s+)*(class|interface|enum|record)\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

// receiverParent extracts the bare type name from a Go method receiver such
// as "*bytes.Buffer" or "s *Server".
func receiverParent(receiver string) string {
	receiver = strings.TrimSpace(receiver)

	if index := strings.LastIndex(receiver, " "); index >= 0 {
		receiver = receiver[index+1:]
	}

	receiver = strings.TrimPrefix(receiver, "*")

	if index := strings.LastIndex(receiver, "."); index >= 0 {
		receiver = receiver[index+1:]
	}

	return receiver
}

func classifyGo(line string) (unitMeta, bool) {
	switch {
	case strings.HasPrefix(line, "package "):
		return unitMeta{symbolType: "package", mergeable: true}, true
	case strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import("):
		return unitMeta{symbolType: "imports", mergeable: true}, true
	case strings.HasPrefix(line, "func "):
		if match := goFuncPattern.FindStringSubmatch(line); match != nil {
			if receiver := strings.TrimSpace(match[1]); receiver != "" {
				name := strings.TrimSpace(strings.SplitN(receiver, " ", 2)[0])
				name = strings.TrimPrefix(name, "*")

				return unitMeta{
					symbolName:   match[2],
					symbolType:   "method",
					parentSymbol: receiverParent(receiver),
				}, true
			}

			return unitMeta{symbolName: match[2], symbolType: "function"}, true
		}

		return unitMeta{symbolType: "function"}, true
	case strings.HasPrefix(line, "type "):
		if match := goTypePattern.FindStringSubmatch(line); match != nil {
			return unitMeta{symbolName: match[1], symbolType: "type_declaration"}, true
		}

		return unitMeta{symbolType: "type_declaration"}, true
	case strings.HasPrefix(line, "const ") || strings.HasPrefix(line, "var "):
		if match := goDeclPattern.FindStringSubmatch(line); match != nil && match[1] != "" {
			kind := "const"
			if strings.HasPrefix(line, "var ") {
				kind = "var"
			}

			return unitMeta{symbolName: match[1], symbolType: kind}, true
		}

		kind := "const"
		if strings.HasPrefix(line, "var ") {
			kind = "var"
		}

		return unitMeta{symbolType: kind}, true
	default:
		return unitMeta{}, false
	}
}

func classifyPython(line string) (unitMeta, bool) {
	switch {
	case strings.HasPrefix(line, "def ") || strings.HasPrefix(line, "async def "):
		if match := pythonFuncPattern.FindStringSubmatch(line); match != nil {
			return unitMeta{symbolName: match[1], symbolType: "function"}, true
		}

		return unitMeta{symbolType: "function"}, true
	case strings.HasPrefix(line, "class "):
		if match := pythonClassPattern.FindStringSubmatch(line); match != nil {
			return unitMeta{symbolName: match[1], symbolType: "class"}, true
		}

		return unitMeta{symbolType: "class"}, true
	case strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "from "):
		return unitMeta{symbolType: "imports", mergeable: true}, true
	default:
		return unitMeta{}, false
	}
}

func classifyJavaScript(line string) (unitMeta, bool) {
	switch {
	case strings.HasPrefix(line, "import "):
		return unitMeta{symbolType: "imports", mergeable: true}, true
	case strings.HasPrefix(line, "export "):
		if match := jsExportPattern.FindStringSubmatch(line); match != nil {
			if match[1] != "" {
				return unitMeta{symbolName: match[1], symbolType: "function"}, true
			}

			if match[2] != "" {
				return unitMeta{symbolName: match[2], symbolType: "class"}, true
			}
		}

		return unitMeta{symbolType: "export"}, true
	case strings.HasPrefix(line, "function ") || strings.HasPrefix(line, "async function "):
		if match := jsFunctionPattern.FindStringSubmatch(line); match != nil {
			return unitMeta{symbolName: match[1], symbolType: "function"}, true
		}

		return unitMeta{symbolType: "function"}, true
	case strings.HasPrefix(line, "class "):
		if match := jsClassPattern.FindStringSubmatch(line); match != nil {
			return unitMeta{symbolName: match[1], symbolType: "class"}, true
		}

		return unitMeta{symbolType: "class"}, true
	default:
		return unitMeta{}, false
	}
}

func classifyJava(line string) (unitMeta, bool) {
	if match := javaTypePattern.FindStringSubmatch(line); match != nil {
		return unitMeta{symbolName: match[2], symbolType: match[1]}, true
	}

	return unitMeta{}, false
}

var (
	goRules = languageRules{
		classify:       classifyGo,
		absorbPrefixes: []string{"//"},
	}

	pythonRules = languageRules{
		classify:       classifyPython,
		absorbPrefixes: []string{"#"},
	}

	javaScriptRules = languageRules{
		classify:       classifyJavaScript,
		absorbPrefixes: []string{"//"},
	}

	javaRules = languageRules{
		classify:       classifyJava,
		absorbPrefixes: []string{"//", "@"},
	}
)

// buildUnits segments lines at structural boundaries. Lines before the first
// boundary form a preamble unit; contiguous comment/annotation lines directly
// above a boundary travel WITH that boundary so doc comments stay attached to
// their declaration. Consecutive mergeable boundaries (imports/package) form
// one unit.
func buildUnits(rules *languageRules, lines []string) []unit {
	var units []unit

	unitStart := 0
	openMeta := unitMeta{}
	lastBoundary := -10

	flush := func(endExcl int) {
		if endExcl > unitStart {
			units = append(units, unit{start: unitStart, endExcl: endExcl, meta: openMeta})
		}
	}

	for index, line := range lines {
		meta, ok := rules.classify(line)

		if !ok {
			continue
		}

		if meta.mergeable && openMeta.mergeable &&
			meta.symbolType == openMeta.symbolType &&
			index == lastBoundary+1 {
			lastBoundary = index

			continue
		}

		start := index

		for back := index - 1; back >= unitStart; back-- {
			if !hasAnyPrefix(lines[back], rules.absorbPrefixes) {
				break
			}

			start = back
		}

		flush(start)

		unitStart = start
		openMeta = meta
		lastBoundary = index
	}

	flush(len(lines))

	return units
}

func hasAnyPrefix(line string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}
