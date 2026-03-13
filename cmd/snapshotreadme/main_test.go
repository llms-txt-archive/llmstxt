package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRendersTemplateWithReleaseHistory(t *testing.T) {
	tempDir := t.TempDir()
	templatePath := filepath.Join(tempDir, "README.tmpl")
	outputPath := filepath.Join(tempDir, "README.md")
	releasesPath := filepath.Join(tempDir, "releases.json")

	templateBody := `# {{ .Title }}
{{ .SiteName }}
{{ .DocumentCount }}
{{ .SkippedCount }}
{{ range .Releases }}- {{ .TagName }} {{ .PublishedLabel }} {{ .HTMLURL }}
{{ end }}`
	releasesBody := `[
  {
    "name": "Initial release",
    "tag_name": "initial",
    "published_at": "2026-03-13T19:42:00Z",
    "html_url": "https://github.com/f-pisani/example/releases/tag/initial"
  }
]`

	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}
	if err := os.WriteFile(releasesPath, []byte(releasesBody), 0o644); err != nil {
		t.Fatalf("WriteFile(releases) error = %v", err)
	}

	cfg := config{
		templatePath:  templatePath,
		outputPath:    outputPath,
		title:         "Claude Code Docs Archive",
		siteName:      "Claude Code Docs",
		siteURL:       "https://code.claude.com",
		sourceURL:     "https://code.claude.com/docs/llms.txt",
		scheduleLabel: "Hourly at :42 UTC",
		documentCount: 64,
		skippedCount:  0,
		releasesPath:  releasesPath,
	}

	if err := run(cfg); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	rendered, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(output) error = %v", err)
	}

	got := string(rendered)
	for _, snippet := range []string{
		"# Claude Code Docs Archive",
		"Claude Code Docs",
		"64",
		"initial",
		"2026-03-13 19:42 UTC",
		"https://github.com/f-pisani/example/releases/tag/initial",
	} {
		if !strings.Contains(got, snippet) {
			t.Fatalf("rendered README missing %q\n%s", snippet, got)
		}
	}
}

func TestFormatPublishedLabel(t *testing.T) {
	got := formatPublishedLabel("2026-03-13T19:42:00Z")
	want := "2026-03-13 19:42 UTC"

	if got != want {
		t.Fatalf("formatPublishedLabel() = %q, want %q", got, want)
	}
}
