// Package filtering implements Task 3c: deterministic, read-only selection of
// the files from an Aevor repository workspace that are suitable for future
// codebase analysis and AI processing.
//
// The package is POLICY + one bounded traversal. It reuses the workspace
// ownership/containment machinery from internal/workspace (the caller passes
// a verified workspace directory) and the language classification table from
// internal/discovery (LanguageForExtension) so both layers can never diverge.
//
// Hard security rules, identical in spirit to discovery (Task 3b):
//
//   - Repository content is UNTRUSTED input. Nothing is executed, no script
//     is evaluated, no dependency is installed, no build tool runs. The
//     walker only stats and lists directory entries.
//   - File CONTENTS are never read into memory. Every decision uses the
//     relative path, extension, and Lstat size metadata.
//   - Symlinks are never followed (filepath.WalkDir Lstat semantics).
//     Symlinked files and directories are counted as skipped and pruned, so
//     a malicious link can neither leak server files into results nor pull
//     the traversal outside the workspace.
//   - Only REPOSITORY-RELATIVE slash paths ever leave this package.
package filtering

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/Aevor/platform/services/api/internal/discovery"
)

// File categories for included files. Excluded files carry no category.
const (
	CategorySource = "source"
	CategoryConfig = "config"
	CategoryDocs   = "documentation"
)

// Decision reasons. Included reasons start with "included_", excluded
// reasons otherwise. The vocabulary is deliberately small, explicit, and
// stable: it is part of the observable API contract.
const (
	ReasonIncludedSource   = "included_source"
	ReasonIncludedConfig   = "included_config"
	ReasonIncludedDocs     = "included_documentation"
	ReasonIgnoredDirectory = "ignored_directory"
	ReasonIgnoredExtension = "ignored_extension"
	ReasonBinary           = "binary"
	ReasonGenerated        = "generated"
	ReasonTooLarge         = "too_large"
	ReasonUnsupported      = "unsupported"
	ReasonSecret           = "secret"
	ReasonTotalSizeLimit   = "total_size_limit"
	ReasonFileCountLimit   = "file_count_limit"
)

// ValidRelativePath reports whether relativePath is a safe repository-relative
// path: non-empty, not absolute, no ".." segment, no "." segments beyond the
// walker's own output, forward slashes only. The traversal cannot produce a
// path that fails this check; it exists as defense in depth so a hostile
// entry name can never smuggle a traversal into decisions or responses.
func ValidRelativePath(relativePath string) bool {
	if relativePath == "" {
		return false
	}

	if strings.HasPrefix(relativePath, "/") || filepath.IsAbs(relativePath) {
		return false
	}

	if strings.Contains(relativePath, "\x00") {
		return false
	}

	for _, segment := range strings.Split(relativePath, "/") {
		switch segment {
		case "", ".", "..":
			// "" catches accidental double slashes and a trailing slash.
			return false
		}
	}

	return true
}

// Decision is the classification result for ONE candidate file. It contains
// no content — only identity metadata and the policy outcome.
type Decision struct {
	Included  bool
	Category  string // CategorySource / CategoryConfig / CategoryDocs; empty when excluded
	Reason    string // always populated (see the Reason constants)
	Extension string // lowercased with dot; empty when the name has none
	Language  string // programming-language label when known; otherwise empty
}

// Classify applies the deterministic include/exclude policy to ONE candidate
// identified by its validated repository-relative path and Lstat size.
// maxFileSize must be > 0; the caller's Options guarantee that.
//
// Evaluation order is fixed and documented:
//
//  1. secret risk (env files, private keys, credential stores)
//  2. per-file size limit ("too_large" — never silently truncated)
//  3. known binary families (archives/images/video/audio/db/compiled/fonts/docs-as-binary)
//  4. generated artifacts (lockfiles, minified bundles, source maps, snapshots)
//  5. known junk text extensions (logs, temp/backup/cache files, vector images)
//  6. configuration by exact/prefix name (Makefile, Dockerfile, go.mod,
//     requirements.txt, CMakeLists.txt, dotfiles like .gitignore/.editorconfig)
//  7. documentation by exact/prefix name (README*, LICENSE*, CONTRIBUTING*...)
//  8. documentation/config/source by extension
//  9. everything else is "unsupported"
//
// Name rules deliberately run before extension rules so important manifests
// that carry generic text extensions stay in their true category
// (requirements.txt → config, CMakeLists.txt → config) while install.sh stays
// source rather than matching the "install" documentation prefix.
func Classify(relativePath string, size int64, maxFileSize int64) Decision {
	slashed := filepath.ToSlash(relativePath)
	base := path.Base(slashed)
	lowerBase := strings.ToLower(base)
	extension := strings.ToLower(filepath.Ext(base))

	if !ValidRelativePath(slashed) {
		// Unreachable via the walker (defense in depth): fail CLOSED.
		return Decision{Included: false, Reason: ReasonUnsupported}
	}

	if isSecret(lowerBase, extension) {
		return Decision{Included: false, Reason: ReasonSecret, Extension: extension}
	}

	if size > maxFileSize {
		return Decision{Included: false, Reason: ReasonTooLarge, Extension: extension}
	}

	if _, binary := binaryExtensions[extension]; binary {
		return Decision{Included: false, Reason: ReasonBinary, Extension: extension}
	}

	if _, generated := generatedNames[lowerBase]; generated || hasAnySuffix(lowerBase, generatedSuffixes) {
		return Decision{Included: false, Reason: ReasonGenerated, Extension: extension}
	}

	if _, junk := junkExtensions[extension]; junk {
		return Decision{Included: false, Reason: ReasonIgnoredExtension, Extension: extension}
	}

	if _, junk := junkNames[lowerBase]; junk {
		return Decision{Included: false, Reason: ReasonIgnoredExtension, Extension: extension}
	}

	if _, config := configNames[lowerBase]; config || matchesPrefix(lowerBase, configNamePrefixes) {
		return Decision{
			Included:  true,
			Category:  CategoryConfig,
			Reason:    ReasonIncludedConfig,
			Extension: extension,
			Language:  languageLabel(extension),
		}
	}

	if matchesDocName(lowerBase) {
		return Decision{Included: true, Category: CategoryDocs, Reason: ReasonIncludedDocs, Extension: extension}
	}

	if _, docs := documentationExtensions[extension]; docs {
		return Decision{Included: true, Category: CategoryDocs, Reason: ReasonIncludedDocs, Extension: extension}
	}

	if _, config := configExtensions[extension]; config {
		return Decision{
			Included:  true,
			Category:  CategoryConfig,
			Reason:    ReasonIncludedConfig,
			Extension: extension,
			Language:  languageLabel(extension),
		}
	}

	if language, source := discovery.LanguageForExtension(extension); source {
		return Decision{Included: true, Category: CategorySource, Reason: ReasonIncludedSource, Extension: extension, Language: language}
	}

	if language, source := additionalSourceLanguages[extension]; source {
		return Decision{Included: true, Category: CategorySource, Reason: ReasonIncludedSource, Extension: extension, Language: language}
	}

	return Decision{Included: false, Reason: ReasonUnsupported, Extension: extension}
}

// isSecret reports whether a candidate looks like it may hold credentials.
// The list is intentionally conservative: a false EXCLUSION of a real source
// file is cheap, a false INCLUSION of a private key is not. ".env*" covers
// .env plus every environment variant INCLUDING .env.example — example files
// routinely contain real values, so they are excluded too (documented).
func isSecret(lowerBase string, extension string) bool {
	if strings.HasPrefix(lowerBase, ".env") {
		return true
	}

	if _, named := secretNames[lowerBase]; named {
		return true
	}

	if _, ext := secretExtensions[extension]; ext {
		return true
	}

	return false
}

// matchesPrefix reports whether base equals or extends one of the prefixes at
// a word boundary (prefix followed by end-of-name, '.', '-' or '_'), so
// "dockerfile" matches "Dockerfile.web" but "install" does not swallow
// "install.sh".
func matchesPrefix(lowerBase string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if lowerBase == prefix {
			return true
		}

		if strings.HasPrefix(lowerBase, prefix) {
			switch lowerBase[len(prefix)] {
			case '.', '-', '_':
				return true
			}
		}
	}

	return false
}

// matchesDocName applies the documentation name prefixes (README, LICENSE,
// CONTRIBUTING, ...) with the same boundary rule as matchesPrefix.
func matchesDocName(lowerBase string) bool {
	return matchesPrefix(lowerBase, documentationNamePrefixes)
}

// languageLabel resolves a display label for configuration extensions whose
// format doubles as a language (yaml/json/toml/xml). Source languages come
// from the shared discovery table instead.
func languageLabel(extension string) string {
	label, ok := configLanguageLabels[extension]

	if !ok {
		return ""
	}

	return label
}

func hasAnySuffix(lowerBase string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(lowerBase, suffix) {
			return true
		}
	}

	return false
}

// ---------------------------------------------------------------------------
// Policy tables. Small, curated, categorized, and easy to extend — the task
// explicitly forbids an enormous arbitrary language list.
// ---------------------------------------------------------------------------

// secretNames are basenames (lowercased) that commonly hold credentials.
var secretNames = map[string]struct{}{
	"id_rsa":           {},
	"id_dsa":           {},
	"id_ecdsa":         {},
	"id_ed25519":       {},
	".netrc":           {},
	".npmrc":           {},
	".pypirc":          {},
	"htpasswd":         {},
	"credentials.json": {},
	".git-credentials": {},
	".dockercfg":       {},
}

// secretExtensions are file types that are keys/keystores/certificates far
// more often than anything useful for codebase understanding.
var secretExtensions = map[string]struct{}{
	".pem":      {},
	".key":      {},
	".pfx":      {},
	".p12":      {},
	".jks":      {},
	".keystore": {},
	".kdbx":     {},
}

// binaryExtensions group KNOWN binary formats by family (archives, images,
// video, audio, databases, compiled artifacts, fonts, opaque documents).
var binaryExtensions = map[string]struct{}{
	// archives / packages
	".zip": {}, ".tar": {}, ".gz": {}, ".tgz": {}, ".bz2": {}, ".xz": {},
	".7z": {}, ".rar": {}, ".zst": {}, ".jar": {}, ".war": {}, ".ear": {},
	".apk": {}, ".gem": {}, ".whl": {}, ".egg": {}, ".nupkg": {},
	".deb": {}, ".rpm": {}, ".iso": {}, ".img": {}, ".dmg": {},
	// images (raster/icon binaries; textual .svg is handled as junk-text below)
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".bmp": {}, ".ico": {},
	".webp": {}, ".tiff": {}, ".tif": {}, ".icns": {},
	// video / audio
	".mp4": {}, ".avi": {}, ".mov": {}, ".mkv": {}, ".webm": {},
	".mp3": {}, ".wav": {}, ".flac": {}, ".ogg": {}, ".m4a": {},
	// databases
	".sqlite": {}, ".sqlite3": {}, ".db": {}, ".mdb": {}, ".duckdb": {},
	// compiled artifacts
	".o": {}, ".obj": {}, ".so": {}, ".dylib": {}, ".a": {}, ".lib": {},
	".dll": {}, ".exe": {}, ".bin": {}, ".pyc": {}, ".pyo": {},
	".class": {}, ".wasm": {}, ".pdb": {},
	// fonts
	".ttf": {}, ".otf": {}, ".woff": {}, ".woff2": {}, ".eot": {},
	// opaque documents / design sources
	".pdf": {}, ".doc": {}, ".docx": {}, ".xls": {}, ".xlsx": {},
	".ppt": {}, ".pptx": {}, ".psd": {}, ".ai": {}, ".sketch": {},
}

// generatedNames are lockfiles and other machine-generated dependency
// manifests. They ARE informative about dependencies, but they are regenerated
// artifacts (often huge) — excluded as "generated", documented.
var generatedNames = map[string]struct{}{
	"package-lock.json":  {},
	"yarn.lock":          {},
	"pnpm-lock.yaml":     {},
	"packages.lock.json": {},
	"composer.lock":      {},
	"gemfile.lock":       {},
	"cargo.lock":         {},
	"go.sum":             {},
	"poetry.lock":        {},
	"pipfile.lock":       {},
	"pdm.lock":           {},
	"mix.lock":           {},
	"shard.lock":         {},
	"cartfile.resolved":  {},
}

// generatedSuffixes catch minified bundles, source maps, and snapshot dumps
// regardless of their base name.
var generatedSuffixes = []string{
	".min.js",
	".min.css",
	".min.html",
	".map",
	".snap",
}

// junkExtensions are low-signal text formats (logs, temp/backup/cache,
// editor droppings) plus vector images that would otherwise read as markup.
var junkExtensions = map[string]struct{}{
	".log":        {},
	".tmp":        {},
	".temp":       {},
	".swp":        {},
	".swo":        {},
	".bak":        {},
	".orig":       {},
	".rej":        {},
	".cache":      {},
	".crdownload": {},
	".part":       {},
	".svg":        {},
}

// junkNames are extensionless OS/editor droppings.
var junkNames = map[string]struct{}{
	".ds_store":   {},
	"thumbs.db":   {},
	"desktop.ini": {},
}

// configNames are exact basenames (lowercased) treated as configuration.
// Includes the manifests called out as MUST-NOT-EXCLUDE (go.mod, Makefile,
// Dockerfile, Gemfile...) and common repository-hygiene dotfiles.
var configNames = map[string]struct{}{
	"makefile":          {},
	"dockerfile":        {},
	"gemfile":           {},
	"rakefile":          {},
	"procfile":          {},
	"vagrantfile":       {},
	"cmakelists.txt":    {},
	"go.mod":            {},
	"requirements.txt":  {},
	"gradle.properties": {},
	"codeowners":        {},
	".gitignore":        {},
	".gitattributes":    {},
	".dockerignore":     {},
	".editorconfig":     {},
	".nvmrc":            {},
	".prettierrc":       {},
	".eslintrc":         {},
	".babelrc":          {},
	".node-version":     {},
	".python-version":   {},
	".ruby-version":     {},
	".tool-versions":    {},
	".flake8":           {},
}

// configNamePrefixes extend configuration matching across variants
// (Dockerfile.web, build.gradle.kts, settings.gradle...).
var configNamePrefixes = []string{
	"dockerfile",
	"build.gradle",
	"settings.gradle",
}

// documentationNamePrefixes cover README/LICENSE/CONTRIBUTING/... variants
// (README.md, LICENSE-MIT, CONTRIBUTING.md, CHANGELOG...) with the boundary
// rule in matchesPrefix. Ambiguous prefixes like "install" are deliberately
// absent (INSTALL.md is caught by its .md extension; scripts named install.sh
// must stay source).
var documentationNamePrefixes = []string{
	"readme",
	"license",
	"licence",
	"copying",
	"changelog",
	"contributing",
	"notice",
	"authors",
	"contributors",
	"maintainers",
	"security",
	"code_of_conduct",
}

// documentationExtensions are plain-text documentation formats.
var documentationExtensions = map[string]struct{}{
	".md":       {},
	".markdown": {},
	".mdx":      {},
	".rst":      {},
	".adoc":     {},
	".asciidoc": {},
	".txt":      {},
}

// configExtensions are structured configuration/data formats by extension.
var configExtensions = map[string]struct{}{
	".json":       {},
	".yaml":       {},
	".yml":        {},
	".toml":       {},
	".ini":        {},
	".cfg":        {},
	".conf":       {},
	".properties": {},
	".xml":        {},
	".gradle":     {},
	".tfvars":     {},
}

// configLanguageLabels give yaml/json/toml/xml configs a display label so the
// languages aggregate stays meaningful for included configuration.
var configLanguageLabels = map[string]string{
	".yaml": "YAML",
	".yml":  "YAML",
	".json": "JSON",
	".toml": "TOML",
	".xml":  "XML",
}

// additionalSourceLanguages are source formats outside the shared discovery
// table (which intentionally stays programming-language focused). Kept tiny
// and labeled; extend here rather than growing discovery's table.
var additionalSourceLanguages = map[string]string{
	".sql":     "SQL",
	".proto":   "Protobuf",
	".graphql": "GraphQL",
	".vue":     "Vue",
	".svelte":  "Svelte",
	".tf":      "Terraform",
	".hcl":     "HCL",
	".css":     "CSS",
	".scss":    "Sass",
	".less":    "Less",
	".html":    "HTML",
	".htm":     "HTML",
}
