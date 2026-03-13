package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExtractLinksDeduplicatesAndSortsMarkdownLinks(t *testing.T) {
	input := []byte(`# Claude Code Docs

- [Overview](https://code.claude.com/docs/en/overview.md)
- [Quickstart](https://code.claude.com/docs/en/quickstart.md)
- [Overview again](https://code.claude.com/docs/en/overview.md)
`)

	got, err := extractLinks(input)
	if err != nil {
		t.Fatalf("extractLinks() error = %v", err)
	}

	want := []string{
		"https://code.claude.com/docs/en/overview.md",
		"https://code.claude.com/docs/en/quickstart.md",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractLinks() = %#v, want %#v", got, want)
	}
}

func TestExtractLinksFallsBackToPlainURLLists(t *testing.T) {
	input := []byte(`# Plain URL index

https://example.com/docs/intro.md
- https://example.com/docs/setup.md
1. https://example.com/docs/advanced.md
`)

	got, err := extractLinks(input)
	if err != nil {
		t.Fatalf("extractLinks() error = %v", err)
	}

	want := []string{
		"https://example.com/docs/advanced.md",
		"https://example.com/docs/intro.md",
		"https://example.com/docs/setup.md",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractLinks() = %#v, want %#v", got, want)
	}
}

func TestPartitionDocumentURLsSkipsNonMarkdownLinks(t *testing.T) {
	input := []string{
		"https://platform.claude.com",
		"https://platform.claude.com/docs",
		"https://platform.claude.com/docs/en/agent-sdk/overview.md",
		"https://platform.claude.com/docs/en/get-started.md",
		"https://platform.claude.com/llms-full.txt",
	}

	gotDocs, gotSkipped, err := partitionDocumentURLs(input)
	if err != nil {
		t.Fatalf("partitionDocumentURLs() error = %v", err)
	}

	wantDocs := []string{
		"https://platform.claude.com/docs/en/agent-sdk/overview.md",
		"https://platform.claude.com/docs/en/get-started.md",
	}
	wantSkipped := []skippedEntry{
		{URL: "https://platform.claude.com", Reason: nonMarkdownReason},
		{URL: "https://platform.claude.com/docs", Reason: nonMarkdownReason},
		{URL: "https://platform.claude.com/llms-full.txt", Reason: nonMarkdownReason},
	}

	if !reflect.DeepEqual(gotDocs, wantDocs) {
		t.Fatalf("partitionDocumentURLs() docs = %#v, want %#v", gotDocs, wantDocs)
	}
	if !reflect.DeepEqual(gotSkipped, wantSkipped) {
		t.Fatalf("partitionDocumentURLs() skipped = %#v, want %#v", gotSkipped, wantSkipped)
	}
}

func TestIsMarkdownURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "markdown", url: "https://example.com/docs/page.md", want: true},
		{name: "uppercase markdown", url: "https://example.com/docs/page.MD", want: true},
		{name: "text", url: "https://example.com/llms.txt", want: false},
		{name: "root", url: "https://example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isMarkdownURL(tt.url)
			if err != nil {
				t.Fatalf("isMarkdownURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("isMarkdownURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRelativePathForURLRootLayout(t *testing.T) {
	got, err := relativePathForURL("https://code.claude.com/docs/en/overview.md", layoutRoot)
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := "docs/en/overview.md"
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLNestedLayout(t *testing.T) {
	got, err := relativePathForURL("https://code.claude.com/docs/en/overview.md", layoutNested)
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := "pages/code.claude.com/docs/en/overview.md"
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLWithQueryInRootLayout(t *testing.T) {
	got, err := relativePathForURL("https://example.com/docs/page.md?lang=en", layoutRoot)
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := "docs/page__0933497c2075.md"
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestSourcePathForLayout(t *testing.T) {
	if got := sourcePathForLayout(layoutRoot); got != "llms.txt" {
		t.Fatalf("sourcePathForLayout(root) = %q, want %q", got, "llms.txt")
	}
	if got := sourcePathForLayout(layoutNested); got != "source/llms.txt" {
		t.Fatalf("sourcePathForLayout(nested) = %q, want %q", got, "source/llms.txt")
	}
}

func TestWriteManifestAndLoadManifestRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	manifestPath := tempDir + "/manifest.json"

	want := manifest{
		SourceURL:            "https://example.com/llms.txt",
		SourcePath:           "llms.txt",
		SourceSHA256:         "source-hash",
		SourceLastModifiedAt: "2026-03-13T06:36:52Z",
		SourceETag:           "\"etag-source\"",
		DocumentCount:        1,
		SkippedCount:         1,
		Documents: []manifestEntry{
			{
				URL:            "https://example.com/docs/overview.md",
				Path:           "docs/overview.md",
				SHA256:         "doc-hash",
				Bytes:          123,
				LastModifiedAt: "2026-03-13T06:36:52Z",
				ETag:           "\"etag-doc\"",
			},
		},
		Skipped: []skippedEntry{
			{URL: "https://example.com/docs", Reason: nonMarkdownReason},
		},
	}

	if err := writeManifest(manifestPath, want); err != nil {
		t.Fatalf("writeManifest() error = %v", err)
	}

	got, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("loadManifest() error = %v", err)
	}

	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("loadManifest() = %#v, want %#v", *got, want)
	}
}

func TestNormalizeLastModified(t *testing.T) {
	got := normalizeLastModified("Fri, 13 Mar 2026 06:36:52 GMT")
	want := "2026-03-13T06:36:52Z"

	if got != want {
		t.Fatalf("normalizeLastModified() = %q, want %q", got, want)
	}
}

func TestIfModifiedSinceHeader(t *testing.T) {
	got := ifModifiedSinceHeader("2026-03-13T06:36:52Z")
	want := "Fri, 13 Mar 2026 06:36:52 GMT"

	if got != want {
		t.Fatalf("ifModifiedSinceHeader() = %q, want %q", got, want)
	}
}

func TestNormalizeETag(t *testing.T) {
	got := normalizeETag(` W/"abc123" `)
	want := `W/"abc123"`

	if got != want {
		t.Fatalf("normalizeETag() = %q, want %q", got, want)
	}
}

func TestEnsureMarkdownResponseRejectsHTML(t *testing.T) {
	err := ensureMarkdownResponse(
		"200 OK",
		"text/html; charset=utf-8",
		map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		[]byte("<!DOCTYPE html><html><head><title>Not Found</title></head><body></body></html>"),
	)
	if err == nil {
		t.Fatal("ensureMarkdownResponse() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "expected markdown response") {
		t.Fatalf("ensureMarkdownResponse() error = %q, want markdown rejection", err)
	}
}

func TestEnsureMarkdownResponseAcceptsMarkdown(t *testing.T) {
	err := ensureMarkdownResponse(
		"200 OK",
		"text/markdown; charset=utf-8",
		map[string][]string{"Content-Type": {"text/markdown; charset=utf-8"}},
		[]byte("# Overview\n\nReal markdown content.\n"),
	)
	if err != nil {
		t.Fatalf("ensureMarkdownResponse() error = %v", err)
	}
}

func TestEnsureMarkdownResponseAcceptsCapturedSkillsMarkdown(t *testing.T) {
	err := ensureMarkdownResponse(
		"200 OK",
		"",
		nil,
		[]byte(
			"> ## Documentation Index\n"+
				"> Fetch the complete documentation index at: https://code.claude.com/docs/llms.txt\n"+
				"> Use this file to discover all available pages before exploring further.\n\n"+
				"# Extend Claude with skills\n\n"+
				"```python\n"+
				"html = '''<html><head>\n"+
				"<title>Example</title>\n"+
				"</head><body></body></html>'''\n"+
				"```\n",
		),
	)
	if err != nil {
		t.Fatalf("ensureMarkdownResponse() error = %v", err)
	}
}

func TestWriteUnexpectedContentDiagnostic(t *testing.T) {
	diagnosticsDir := t.TempDir()
	unexpected := &unexpectedContentError{
		message:     "expected markdown response but received HTML document",
		status:      "200 OK",
		contentType: "text/html; charset=utf-8",
		headers:     map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		body:        []byte("<!DOCTYPE html><html><body>bad html</body></html>"),
	}

	diagnosticPath, err := writeUnexpectedContentDiagnostic(
		diagnosticsDir,
		"https://example.com/docs/skills.md",
		"docs/skills.md",
		unexpected,
	)
	if err != nil {
		t.Fatalf("writeUnexpectedContentDiagnostic() error = %v", err)
	}

	wantDiagnosticPath := "docs/skills.md.unexpected-content.json"
	if diagnosticPath != wantDiagnosticPath {
		t.Fatalf("writeUnexpectedContentDiagnostic() path = %q, want %q", diagnosticPath, wantDiagnosticPath)
	}

	bodyPath := filepath.Join(diagnosticsDir, "docs", "skills.md.unexpected-content.html")
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read diagnostic body: %v", err)
	}
	if got := string(body); got != string(unexpected.body) {
		t.Fatalf("diagnostic body = %q, want %q", got, string(unexpected.body))
	}

	metadataPath := filepath.Join(diagnosticsDir, filepath.FromSlash(diagnosticPath))
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read diagnostic metadata: %v", err)
	}
	if !strings.Contains(string(metadata), `"body_path": "docs/skills.md.unexpected-content.html"`) {
		t.Fatalf("diagnostic metadata missing body path: %s", string(metadata))
	}
}

func TestFetchDocumentRetriesTransientHTMLForMarkdownURL(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!DOCTYPE html><html><body>temporary html</body></html>"))
		default:
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte("# Real markdown\n"))
		}
	}))
	defer server.Close()

	result, err := fetchDocument(http.DefaultClient, server.URL+"/skills.md", "skills.md", manifestEntry{})
	if err != nil {
		t.Fatalf("fetchDocument() error = %v", err)
	}

	if got := string(result.Body); got != "# Real markdown\n" {
		t.Fatalf("fetchDocument() body = %q, want markdown body", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("fetchDocument() attempts = %d, want 2", got)
	}
}
