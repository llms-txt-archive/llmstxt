package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withReadmeFlagSet(t *testing.T, args []string) {
	t.Helper()

	previousArgs := os.Args
	previousFlagSet := flag.CommandLine
	testFlagSet := flag.NewFlagSet(args[0], flag.ContinueOnError)
	testFlagSet.SetOutput(io.Discard)

	os.Args = args
	flag.CommandLine = testFlagSet

	t.Cleanup(func() {
		os.Args = previousArgs
		flag.CommandLine = previousFlagSet
	})
}

func TestParseFlagsSuccess(t *testing.T) {
	withReadmeFlagSet(t, []string{
		"snapshotreadme",
		"-template", "/tmp/template.md",
		"-out", "/tmp/README.md",
		"-title", "Archive",
		"-site-name", "Example Docs",
		"-site-url", "https://example.com/docs",
		"-source-url", "https://example.com/llms.txt",
		"-schedule-label", "Hourly",
		"-document-count", "42",
		"-skipped-count", "3",
		"-releases-json", "/tmp/releases.json",
	})

	cfg := parseFlags()
	if cfg.templatePath != "/tmp/template.md" {
		t.Fatalf("parseFlags() template = %q", cfg.templatePath)
	}
	if cfg.outputPath != "/tmp/README.md" {
		t.Fatalf("parseFlags() out = %q", cfg.outputPath)
	}
	if cfg.documentCount != 42 {
		t.Fatalf("parseFlags() document_count = %d", cfg.documentCount)
	}
	if cfg.skippedCount != 3 {
		t.Fatalf("parseFlags() skipped_count = %d", cfg.skippedCount)
	}
}

func TestLoadReleasesFormatsPublishedLabels(t *testing.T) {
	releasesPath := filepath.Join(t.TempDir(), "releases.json")
	body := `[
  {"name":"Snapshot A","tag_name":"snapshot-a","published_at":"2026-03-14T10:15:00Z","html_url":"https://example.com/a"},
  {"name":"Snapshot B","tag_name":"snapshot-b","published_at":"not-a-date","html_url":"https://example.com/b"}
]`
	if err := os.WriteFile(releasesPath, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	releases, err := loadReleases(releasesPath)
	if err != nil {
		t.Fatalf("loadReleases() error = %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("loadReleases() len = %d, want 2", len(releases))
	}
	if releases[0].PublishedLabel != "2026-03-14 10:15 UTC" {
		t.Fatalf("loadReleases() label = %q", releases[0].PublishedLabel)
	}
	if releases[1].PublishedLabel != "not-a-date" {
		t.Fatalf("loadReleases() invalid label = %q", releases[1].PublishedLabel)
	}
}

func TestRunRendersTemplate(t *testing.T) {
	tempDir := t.TempDir()
	templatePath := filepath.Join(tempDir, "template.md.tmpl")
	releasesPath := filepath.Join(tempDir, "releases.json")
	outputPath := filepath.Join(tempDir, "README.md")

	templateBody := `# {{ .Title }}
{{ .SiteName }}
{{ .SiteURL }}
{{ .SourceURL }}
{{ .ScheduleLabel }}
docs={{ .DocumentCount }}
skipped={{ .SkippedCount }}
{{ range .Releases }}- {{ .Name }} @ {{ .PublishedLabel }} -> {{ .HTMLURL }}
{{ end }}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o600); err != nil {
		t.Fatalf("os.WriteFile(template) error = %v", err)
	}
	if err := os.WriteFile(releasesPath, []byte(`[{"name":"Initial release","tag_name":"initial","published_at":"2026-03-14T10:15:00Z","html_url":"https://example.com/initial"}]`), 0o600); err != nil {
		t.Fatalf("os.WriteFile(releases) error = %v", err)
	}

	err := run(config{
		templatePath:  templatePath,
		outputPath:    outputPath,
		title:         "Archive Title",
		siteName:      "Example Docs",
		siteURL:       "https://example.com/docs",
		sourceURL:     "https://example.com/llms.txt",
		scheduleLabel: "Hourly at :42 UTC",
		documentCount: 10,
		skippedCount:  1,
		releasesPath:  releasesPath,
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// #nosec G304 -- tests only read temp files they created themselves.
	rendered, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	output := string(rendered)
	for _, expected := range []string{
		"# Archive Title",
		"Example Docs",
		"https://example.com/llms.txt",
		"docs=10",
		"skipped=1",
		"Initial release @ 2026-03-14 10:15 UTC -> https://example.com/initial",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("rendered README missing %q\n%s", expected, output)
		}
	}
}

func TestMainInvokesRunSnapshotReadme(t *testing.T) {
	withReadmeFlagSet(t, []string{
		"snapshotreadme",
		"-template", "/tmp/template.md",
		"-out", "/tmp/README.md",
		"-title", "Archive",
		"-site-name", "Example Docs",
		"-site-url", "https://example.com/docs",
		"-source-url", "https://example.com/llms.txt",
		"-schedule-label", "Hourly",
		"-releases-json", "/tmp/releases.json",
	})

	previousRun := runSnapshotReadme
	t.Cleanup(func() {
		runSnapshotReadme = previousRun
	})

	called := false
	runSnapshotReadme = func(cfg config) error {
		called = true
		if cfg.title != "Archive" {
			t.Fatalf("main() title = %q", cfg.title)
		}
		return nil
	}

	main()

	if !called {
		t.Fatal("main() did not invoke runSnapshotReadme")
	}
}
