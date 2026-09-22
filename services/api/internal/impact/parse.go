package impact

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	interfaceMethodPattern = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	quotedLiteralPattern   = regexp.MustCompile("\"([^\"\\n]+)\"|`([^`\\n]+)`")
)

// extractImportPaths pulls the import/module specifiers out of one file's
// import-relevant text. It is a deliberate lexical heuristic (not a parser):
// it recognizes the conventional import syntax of Go, Python, and
// JavaScript/TypeScript and returns the referenced specifiers verbatim.
func extractImportPaths(language, source string) []string {
	switch language {
	case "Go":
		return extractGoImports(source)
	case "Python":
		return extractPythonImports(source)
	case "JavaScript", "TypeScript":
		return extractJavaScriptImports(source)
	default:
		return nil
	}
}

func extractGoImports(source string) []string {
	var out []string
	inBlock := false

	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)

		if inBlock {
			if strings.HasPrefix(trimmed, ")") {
				inBlock = false
				continue
			}
			if spec := firstQuoted(trimmed); spec != "" {
				out = append(out, spec)
			}
			continue
		}

		if trimmed == "import(" || strings.HasPrefix(trimmed, "import (") {
			inBlock = true
			continue
		}
		if strings.HasPrefix(trimmed, "import ") {
			if spec := firstQuoted(trimmed); spec != "" {
				out = append(out, spec)
			}
		}
	}

	return out
}

func extractPythonImports(source string) []string {
	var out []string

	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "import "):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "import "))
			for _, part := range strings.Split(rest, ",") {
				fields := strings.Fields(part)
				if len(fields) > 0 {
					out = append(out, fields[0])
				}
			}
		case strings.HasPrefix(trimmed, "from "):
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "from "))
			if index := strings.Index(rest, " import"); index >= 0 {
				out = append(out, strings.TrimSpace(rest[:index]))
			}
		}
	}

	return out
}

func extractJavaScriptImports(source string) []string {
	var out []string

	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)

		if !strings.HasPrefix(trimmed, "import ") &&
			!strings.HasPrefix(trimmed, "export ") &&
			!strings.Contains(trimmed, "require(") {
			continue
		}

		for _, match := range quotedLiteralPattern.FindAllStringSubmatch(trimmed, -1) {
			spec := match[1]
			if spec == "" {
				spec = match[2]
			}
			if spec != "" {
				out = append(out, spec)
			}
		}
	}

	return out
}

func firstQuoted(line string) string {
	match := quotedLiteralPattern.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	if match[1] != "" {
		return match[1]
	}
	return match[2]
}

// resolveImport maps one import specifier to the repository files it can
// denote. Unresolvable specifiers (external packages, standard library) yield
// nothing rather than a guess. Results are capped and sorted.
func resolveImport(importerFile, rawImport, language string, dirToFiles map[string][]string, files map[string]struct{}) []string {
	raw := strings.TrimSpace(rawImport)
	if raw == "" {
		return nil
	}

	var resolved []string

	switch language {
	case "JavaScript", "TypeScript":
		if strings.HasPrefix(raw, ".") || strings.HasPrefix(raw, "/") {
			resolved = resolveRelativeFile(importerFile, raw, files)
		}
	case "Python":
		if strings.HasPrefix(raw, ".") {
			resolved = resolvePythonRelative(importerFile, raw, dirToFiles, files)
		} else {
			resolved = resolveBySuffix(strings.Split(raw, "."), dirToFiles)
		}
	case "Go":
		resolved = resolveBySuffix(strings.Split(raw, "/"), dirToFiles)
	}

	sort.Strings(resolved)
	return capFiles(resolved)
}

// resolveBySuffix finds the most specific repository directory matching a
// trailing segment run of an import path (e.g. ".../internal/ai" resolves to
// "internal/ai" when that directory exists) and returns its direct files.
func resolveBySuffix(segments []string, dirToFiles map[string][]string) []string {
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment != "" && segment != "." {
			cleaned = append(cleaned, segment)
		}
	}
	for length := len(cleaned); length >= 1; length-- {
		candidate := strings.Join(cleaned[len(cleaned)-length:], "/")
		if files := dirToFiles[candidate]; len(files) > 0 {
			return append([]string(nil), files...)
		}
	}
	return nil
}

// resolveRelativeFile resolves a JS/TS relative import to a concrete file,
// trying the exact path plus conventional extensions and directory indexes.
func resolveRelativeFile(importerFile, raw string, files map[string]struct{}) []string {
	base := dirOf(importerFile)
	target := path.Clean(path.Join(base, raw))
	return matchFileOrIndex(target, files)
}

// resolvePythonRelative resolves a dotted relative Python import, where N
// leading dots climb N-1 directories from the importing package.
func resolvePythonRelative(importerFile, raw string, dirToFiles map[string][]string, files map[string]struct{}) []string {
	leading := 0
	for leading < len(raw) && raw[leading] == '.' {
		leading++
	}
	remainder := strings.TrimLeft(raw, ".")

	base := dirOf(importerFile)
	for climb := 1; climb < leading; climb++ {
		base = path.Dir(base)
		if base == "/" {
			base = "."
		}
	}

	dotted := strings.TrimPrefix(remainder, ".")
	segments := make([]string, 0)
	for _, segment := range strings.Split(dotted, ".") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}

	target := base
	if len(segments) > 0 {
		target = path.Clean(path.Join(base, strings.Join(segments, "/")))
	}

	if files := dirToFiles[target]; len(files) > 0 {
		return append([]string(nil), files...)
	}
	return matchFileOrIndex(target, files)
}

func matchFileOrIndex(target string, files map[string]struct{}) []string {
	extensions := []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".go"}

	for _, extension := range extensions {
		if _, ok := files[target+extension]; ok {
			return []string{target + extension}
		}
	}
	for _, extension := range extensions {
		index := target + "/index" + extension
		if _, ok := files[index]; ok {
			return []string{index}
		}
	}
	return nil
}

func capFiles(files []string) []string {
	if len(files) <= maxImportTargetsPerImport {
		return files
	}
	return files[:maxImportTargetsPerImport]
}

// identifiersIn tokenizes source text into identifier names mapped to whether
// at least one occurrence is immediately followed by "(" (a lexical call
// signal). The scanner is language-agnostic and deliberately simple.
func identifiersIn(content string) map[string]bool {
	out := make(map[string]bool)
	if content == "" {
		return out
	}

	i := 0
	for i < len(content) {
		if !isIdentStart(content[i]) {
			i++
			continue
		}

		start := i
		i++
		for i < len(content) && isIdentPart(content[i]) {
			i++
		}

		name := content[start:i]
		j := i
		for j < len(content) && (content[j] == ' ' || content[j] == '\t') {
			j++
		}
		isCall := j < len(content) && content[j] == '('

		if previous, ok := out[name]; !ok || (isCall && !previous) {
			out[name] = isCall || previous
		}
	}

	return out
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// interfaceMethodNames extracts the method names declared by an interface
// body. Embedded interfaces (a bare type name) are ignored: they carry no
// method-set evidence of their own.
func interfaceMethodNames(content string) []string {
	index := strings.Index(content, "interface")
	if index < 0 {
		return nil
	}
	open := strings.Index(content[index:], "{")
	if open < 0 {
		return nil
	}

	body := content[index+open+1:]
	if close := strings.Index(body, "}"); close >= 0 {
		body = body[:close]
	}

	segments := strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == ';' })
	set := make(map[string]bool)
	for _, segment := range segments {
		if match := interfaceMethodPattern.FindStringSubmatch(segment); match != nil {
			set[match[1]] = true
		}
	}

	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}
