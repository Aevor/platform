package discovery

import "strings"

// LanguageForExtension maps a file extension (with dot, any case) to a
// programming-language label. Deliberately PROGRAMMING languages only:
// markup, documentation, and configuration formats (md/json/yaml/toml/...)
// are excluded so they cannot drown out code signals. Unknown and binary
// extensions yield no language.
//
// Exported so the Task 3c filtering layer classifies languages from the SAME
// table instead of maintaining a divergent copy.
func LanguageForExtension(extension string) (string, bool) {
	if extension == "" {
		return "", false
	}

	language, known := languagesByExtension[strings.ToLower(extension)]

	return language, known
}

var languagesByExtension = map[string]string{
	".go":    "Go",
	".js":    "JavaScript",
	".mjs":   "JavaScript",
	".cjs":   "JavaScript",
	".jsx":   "JavaScript",
	".ts":    "TypeScript",
	".tsx":   "TypeScript",
	".py":    "Python",
	".rb":    "Ruby",
	".rs":    "Rust",
	".java":  "Java",
	".kt":    "Kotlin",
	".kts":   "Kotlin",
	".swift": "Swift",
	".c":     "C",
	".h":     "C",
	".cpp":   "C++",
	".cc":    "C++",
	".cxx":   "C++",
	".hpp":   "C++",
	".cs":    "C#",
	".php":   "PHP",
	".scala": "Scala",
	".dart":  "Dart",
	".lua":   "Lua",
	".pl":    "Perl",
	".r":     "R",
	".m":     "Objective-C",
	".sh":    "Shell",
	".bash":  "Shell",
}
