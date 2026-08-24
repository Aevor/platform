package filtering

import (
	"testing"
)

func TestValidRelativePath(t *testing.T) {
	cases := map[string]bool{
		"main.go":                  true,
		"cmd/server/main.go":       true,
		".github/workflows/ci.yml": true,
		"a b/c.txt":                true, // spaces are legal filename content
		"":                         false,
		"/etc/passwd":              false,
		"..":                       false,
		"../etc/passwd":            false,
		"main/../../../etc/passwd": false,
		"a/..":                     false,
		"./main.go":                false,
		"src//main.go":             false,
		"src/":                     false,
		"main\x00.go":              false,
		"C:/evil":                  true, // a legal (odd) unix directory name; containment is structural
	}

	for candidate, want := range cases {
		if got := ValidRelativePath(candidate); got != want {
			t.Errorf("ValidRelativePath(%q) = %v, want %v", candidate, got, want)
		}
	}
}

func TestClassify_IncludedSources(t *testing.T) {
	cases := []struct {
		path     string
		category string
		reason   string
		language string
	}{
		{"main.go", CategorySource, ReasonIncludedSource, "Go"},
		{"cmd/server/main.go", CategorySource, ReasonIncludedSource, "Go"},
		{"web/app.tsx", CategorySource, ReasonIncludedSource, "TypeScript"},
		{"scripts/deploy.sh", CategorySource, ReasonIncludedSource, "Shell"},
		{"db/schema.sql", CategorySource, ReasonIncludedSource, "SQL"},
		{"api/proto.proto", CategorySource, ReasonIncludedSource, "Protobuf"},
		{"web/style.css", CategorySource, ReasonIncludedSource, "CSS"},
		{"web/page.html", CategorySource, ReasonIncludedSource, "HTML"},
	}

	for _, c := range cases {
		decision := Classify(c.path, 100, 1024)

		if !decision.Included || decision.Category != c.category || decision.Reason != c.reason {
			t.Errorf("Classify(%q) = %+v, want included %s/%s", c.path, decision, c.category, c.reason)
		}

		if decision.Language != c.language && !(c.language == "" && decision.Language == "") {
			t.Errorf("Classify(%q) language = %q, want %q", c.path, decision.Language, c.language)
		}
	}
}

func TestClassify_CSSIsSource(t *testing.T) {
	// .css is frontend code evidence; it must not fall through to unsupported.
	// It has no entry in either language table, so the language stays empty
	// while the file is still INCLUDED as source.
	decision := Classify("assets/app.css", 10, 1024)

	if !decision.Included || decision.Reason != ReasonIncludedSource {
		t.Errorf("css classified as %+v, want included_source", decision)
	}
}

func TestClassify_DocumentationIncluded(t *testing.T) {
	cases := []string{
		"README.md",
		"readme.rst",
		"LICENSE",
		"LICENSE-MIT",
		"CONTRIBUTING.md",
		"CHANGELOG.md",
		"docs/guide.md",
		"docs/manual.adoc",
		"NOTICE",
		"SECURITY.md",
		"code_of_conduct.txt",
	}

	for _, path := range cases {
		decision := Classify(path, 100, 1024)

		if !decision.Included || decision.Category != CategoryDocs || decision.Reason != ReasonIncludedDocs {
			t.Errorf("Classify(%q) = %+v, want included_documentation", path, decision)
		}
	}
}

func TestClassify_ConfigurationIncluded(t *testing.T) {
	cases := []struct {
		path     string
		language string
	}{
		{"package.json", "JSON"},
		{"go.mod", ""},
		{"Cargo.toml", "TOML"},
		{"pyproject.toml", "TOML"},
		{"pom.xml", "XML"},
		{"build.gradle", ""},
		{"build.gradle.kts", ""},
		{"settings.gradle", ""},
		{"Dockerfile", ""},
		{"Dockerfile.web", ""},
		{"Makefile", ""},
		{"docker-compose.yml", "YAML"},
		{".github/workflows/ci.yml", "YAML"},
		{"requirements.txt", ""}, // manifest name beats .txt documentation
		{"CMakeLists.txt", ""},   // build config name beats .txt documentation
		{"tsconfig.json", "JSON"},
		{".gitignore", ""},
		{".editorconfig", ""},
		{"config/app.conf", ""},
		{"gradle.properties", ""},
	}

	for _, c := range cases {
		decision := Classify(c.path, 100, 1024)

		if !decision.Included || decision.Category != CategoryConfig || decision.Reason != ReasonIncludedConfig {
			t.Errorf("Classify(%q) = %+v, want included_config", c.path, decision)
		}

		if decision.Language != c.language {
			t.Errorf("Classify(%q) language = %q, want %q", c.path, decision.Language, c.language)
		}
	}
}

func TestClassify_SourceLanguagesAreLabeled(t *testing.T) {
	// Every included_source decision must carry a non-empty language label:
	// the languages aggregate in the result is built from these.
	for _, path := range []string{"a.go", "b.tsx", "c.py", "d.sh", "e.sql", "f.css", "g.html"} {
		decision := Classify(path, 10, 1024)

		if !decision.Included || decision.Reason != ReasonIncludedSource {
			t.Errorf("Classify(%q) = %+v, want included_source", path, decision)
		}

		if decision.Language == "" {
			t.Errorf("Classify(%q) produced an empty language label", path)
		}
	}
}

func TestClassify_InstallShStaysSource(t *testing.T) {
	// The "install" documentation prefix must not swallow scripts.
	decision := Classify("scripts/install.sh", 100, 1024)

	if !decision.Included || decision.Category != CategorySource || decision.Language != "Shell" {
		t.Errorf("install.sh = %+v, want included Shell source", decision)
	}
}

func TestClassify_Exclusions(t *testing.T) {
	cases := []struct {
		path   string
		size   int64
		reason string
	}{
		// NOTE: ignored directories (.git, node_modules, ...) are PRUNED by the
		// walk and never reach Classify — covered by the service tests below.
		{".env", 10, ReasonSecret},
		{".env.production", 10, ReasonSecret},
		{".env.example", 10, ReasonSecret}, // deliberate: example files often hold real values
		{"deploy/id_rsa", 10, ReasonSecret},
		{"server.pem", 10, ReasonSecret},
		{"cert.key", 10, ReasonSecret},
		{"keystore.jks", 10, ReasonSecret},
		{"credentials.json", 10, ReasonSecret},
		{".npmrc", 10, ReasonSecret},
		{"logo.png", 10, ReasonBinary},
		{"bundle.tar.gz", 10, ReasonBinary},
		{"app.exe", 10, ReasonBinary},
		{"data.sqlite3", 10, ReasonBinary},
		{"font.woff2", 10, ReasonBinary},
		{"doc.pdf", 10, ReasonBinary},
		{"video.mp4", 10, ReasonBinary},
		{"package-lock.json", 10, ReasonGenerated},
		{"yarn.lock", 10, ReasonGenerated},
		{"go.sum", 10, ReasonGenerated},
		{"Cargo.lock", 10, ReasonGenerated},
		{"app.min.js", 10, ReasonGenerated},
		{"bundle.js.map", 10, ReasonGenerated},
		{"__snapshots__/ui.snap", 10, ReasonGenerated},
		{"debug.log", 10, ReasonIgnoredExtension},
		{"backup.bak", 10, ReasonIgnoredExtension},
		{"icon.svg", 10, ReasonIgnoredExtension},
		{".DS_Store", 10, ReasonIgnoredExtension},
		{"huge.md", 2048, ReasonTooLarge}, // size limit wins over documentation value
		{"plugin.foo", 10, ReasonUnsupported},
		{"artifact", 10, ReasonUnsupported}, // extensionless, no known name
		{"data.csv", 10, ReasonUnsupported},
	}

	for _, c := range cases {
		decision := Classify(c.path, c.size, 1024)

		if decision.Included {
			t.Errorf("Classify(%q, %d) unexpectedly INCLUDED (%+v)", c.path, c.size, decision)
			continue
		}

		if decision.Reason != c.reason {
			t.Errorf("Classify(%q, %d) reason = %q, want %q", c.path, c.size, decision.Reason, c.reason)
		}
	}
}

func TestClassify_SizeLimitBeatsEverythingBelowIt(t *testing.T) {
	// Order check: a huge binary reports too_large only if the size gate runs
	// BEFORE the binary table — verify both orders resolve deterministically.
	bigBinary := Classify("asset.png", 9999, 1024)
	bigSecret := Classify("leak.pem", 9999, 1024)

	if bigBinary.Reason != ReasonTooLarge {
		t.Errorf("oversized binary reason = %q, want too_large", bigBinary.Reason)
	}

	// Security outranks size: an oversized key is still reported as a secret.
	if bigSecret.Reason != ReasonSecret {
		t.Errorf("oversized secret reason = %q, want secret", bigSecret.Reason)
	}
}

func TestClassify_CaseInsensitive(t *testing.T) {
	if d := Classify("README.MD", 10, 1024); !d.Included || d.Reason != ReasonIncludedDocs {
		t.Errorf("uppercase README.MD = %+v", d)
	}

	if d := Classify("Makefile", 10, 1024); !d.Included || d.Reason != ReasonIncludedConfig {
		t.Errorf("Makefile = %+v", d)
	}

	if d := Classify("MAKEFILE", 10, 1024); !d.Included || d.Reason != ReasonIncludedConfig {
		t.Errorf("uppercase MAKEFILE = %+v", d)
	}

	if d := Classify("LOGO.PNG", 10, 1024); d.Included || d.Reason != ReasonBinary {
		t.Errorf("uppercase PNG = %+v", d)
	}

	if d := Classify("Main.GO", 10, 1024); !d.Included || d.Language != "Go" {
		t.Errorf("mixed-case .GO = %+v", d)
	}
}

func TestClassify_TraversalFailsClosed(t *testing.T) {
	for _, hostile := range []string{"../outside.txt", "/abs/path.md", "a/../b.go"} {
		decision := Classify(hostile, 10, 1024)

		if decision.Included {
			t.Errorf("hostile path %q was INCLUDED", hostile)
		}

		if decision.Category != "" {
			t.Errorf("hostile path %q carried category %q", hostile, decision.Category)
		}
	}
}
