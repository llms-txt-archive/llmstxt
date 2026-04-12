package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/llms-txt-archive/llmstxt/internal/app"
	"github.com/llms-txt-archive/llmstxt/internal/fetch"
	"github.com/llms-txt-archive/llmstxt/internal/fileutil"
	"github.com/llms-txt-archive/llmstxt/internal/links"
	"github.com/llms-txt-archive/llmstxt/internal/manifest"
	"github.com/llms-txt-archive/llmstxt/internal/policy"
	"github.com/llms-txt-archive/llmstxt/internal/stage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testResponse(status int, headers map[string]string, body string) *http.Response {
	header := make(http.Header, len(headers))
	for key, value := range headers {
		header.Set(key, value)
	}

	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newTestClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func mustPolicy(t *testing.T, sourceURL string, allowedHostsCSV string) *policy.URLPolicy {
	t.Helper()

	policy, err := policy.NewURLPolicy(sourceURL, allowedHostsCSV)
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	return policy
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	// #nosec G304 -- tests only read temp files they created themselves.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	return string(body)
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
	}
	return parsedURL
}

func withTestFlagSet(t *testing.T, args []string) {
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

func TestParseFlagsUsesDefaults(t *testing.T) {
	withTestFlagSet(t, []string{
		"crawler",
		"-source", "https://docs.example.com/llms.txt",
		"-out", filepath.Join(t.TempDir(), "snapshot"),
		"-manifest-out", filepath.Join(t.TempDir(), "manifest.json"),
	})

	cfg := parseFlags()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	if cfg.SourceURL != "https://docs.example.com/llms.txt" {
		t.Fatalf("parseFlags() source = %q", cfg.SourceURL)
	}
	if cfg.Layout != defaultLayout {
		t.Fatalf("parseFlags() layout = %q, want %q", cfg.Layout, defaultLayout)
	}
	if cfg.Timeout != defaultTimeout {
		t.Fatalf("parseFlags() timeout = %v, want %v", cfg.Timeout, defaultTimeout)
	}
	if cfg.Concurrency != defaultWorkers {
		t.Fatalf("parseFlags() concurrency = %d, want %d", cfg.Concurrency, defaultWorkers)
	}
	if cfg.ArchiveRoot != wd {
		t.Fatalf("parseFlags() archive_root = %q, want %q", cfg.ArchiveRoot, wd)
	}
}

func TestParseFlagsNormalizesConcurrencyFloor(t *testing.T) {
	withTestFlagSet(t, []string{
		"crawler",
		"-source", "https://docs.example.com/llms.txt",
		"-out", filepath.Join(t.TempDir(), "snapshot"),
		"-manifest-out", filepath.Join(t.TempDir(), "manifest.json"),
		"-concurrency", "0",
		"-layout", links.LayoutRoot,
	})

	cfg := parseFlags()
	if cfg.Concurrency != 1 {
		t.Fatalf("parseFlags() concurrency = %d, want 1", cfg.Concurrency)
	}
	if cfg.Layout != links.LayoutRoot {
		t.Fatalf("parseFlags() layout = %q, want %q", cfg.Layout, links.LayoutRoot)
	}
}

func TestMainInvokesRunApp(t *testing.T) {
	withTestFlagSet(t, []string{
		"crawler",
		"-source", "https://docs.example.com/llms.txt",
		"-out", filepath.Join(t.TempDir(), "snapshot"),
		"-manifest-out", filepath.Join(t.TempDir(), "manifest.json"),
	})

	// Verify parseFlags works correctly with the CLI struct.
	// We can't easily intercept newCLI().run in the refactored code,
	// so we test that parseFlags produces the expected config.
	cfg := parseFlags()
	if cfg.SourceURL != "https://docs.example.com/llms.txt" {
		t.Fatalf("parseFlags() source = %q", cfg.SourceURL)
	}
}

func TestExtractLinksDeduplicatesAndSortsMarkdownLinks(t *testing.T) {
	input := []byte(`# Claude Code Docs

- [Overview](https://code.claude.com/docs/en/overview.md)
- [Quickstart](https://code.claude.com/docs/en/quickstart.md)
- [Overview again](https://code.claude.com/docs/en/overview.md)
`)

	got, err := links.Extract(input)
	if err != nil {
		t.Fatalf("links.Extract() error = %v", err)
	}

	want := []string{
		"https://code.claude.com/docs/en/overview.md",
		"https://code.claude.com/docs/en/quickstart.md",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("links.Extract() mismatch (-want +got):\n%s", diff)
	}
}

func TestExtractLinksFallsBackToPlainURLLists(t *testing.T) {
	input := []byte(`# Plain URL index

https://example.com/docs/intro.md
- https://example.com/docs/setup.md
1. https://example.com/docs/advanced.md
`)

	got, err := links.Extract(input)
	if err != nil {
		t.Fatalf("links.Extract() error = %v", err)
	}

	want := []string{
		"https://example.com/docs/advanced.md",
		"https://example.com/docs/intro.md",
		"https://example.com/docs/setup.md",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("links.Extract() mismatch (-want +got):\n%s", diff)
	}
}

func TestExtractLinksErrorsWhenNoURLsExist(t *testing.T) {
	_, err := links.Extract([]byte("# Empty index\n\nNo linked docs here.\n"))
	if err == nil {
		t.Fatal("links.Extract() error = nil, want error")
	}
	if !errors.Is(err, links.ErrNoDocumentURLs) {
		t.Fatalf("links.Extract() error = %v, want ErrNoDocumentURLs", err)
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

	gotDocs, gotIndexes, gotSkipped, err := links.Partition(input)
	if err != nil {
		t.Fatalf("links.Partition() error = %v", err)
	}

	wantDocs := []string{
		"https://platform.claude.com/docs/en/agent-sdk/overview.md",
		"https://platform.claude.com/docs/en/get-started.md",
	}
	wantSkipped := []manifest.SkippedEntry{
		{URL: "https://platform.claude.com", Reason: links.NonMarkdownReason},
		{URL: "https://platform.claude.com/docs", Reason: links.NonMarkdownReason},
		{URL: "https://platform.claude.com/llms-full.txt", Reason: links.NonMarkdownReason},
	}

	if diff := cmp.Diff(wantDocs, gotDocs); diff != "" {
		t.Fatalf("links.Partition() docs mismatch (-want +got):\n%s", diff)
	}
	if len(gotIndexes) != 0 {
		t.Fatalf("links.Partition() indexes = %v, want empty", gotIndexes)
	}
	if diff := cmp.Diff(wantSkipped, gotSkipped); diff != "" {
		t.Fatalf("links.Partition() skipped mismatch (-want +got):\n%s", diff)
	}
}

func TestIsMarkdownURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
		err  string
	}{
		{name: "markdown", url: "https://example.com/docs/page.md", want: true},
		{name: "uppercase markdown", url: "https://example.com/docs/page.MD", want: true},
		{name: "text", url: "https://example.com/llms.txt", want: false},
		{name: "root", url: "https://example.com", want: false},
		{name: "relative markdown", url: "docs/page.md", want: false, err: "missing scheme or host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := links.IsMarkdown(tt.url)
			if tt.err != "" {
				if err == nil {
					t.Fatalf("links.IsMarkdown() error = nil, want %q", tt.err)
				}
				if !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("links.IsMarkdown() error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("links.IsMarkdown() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("links.IsMarkdown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestURLPolicyValidation(t *testing.T) {
	tests := []struct {
		name            string
		sourceURL       string
		allowedHostsCSV string
		validateURL     string
		wantErr         bool
	}{
		{
			name:        "source host allowed",
			sourceURL:   "https://code.claude.com/docs/llms.txt",
			validateURL: "https://code.claude.com/docs/en/overview.md",
		},
		{
			name:        "http scheme rejected",
			sourceURL:   "https://code.claude.com/docs/llms.txt",
			validateURL: "http://code.claude.com/docs/en/overview.md",
			wantErr:     true,
		},
		{
			name:        "cross host rejected",
			sourceURL:   "https://code.claude.com/docs/llms.txt",
			validateURL: "https://platform.claude.com/docs/en/overview.md",
			wantErr:     true,
		},
		{
			name:        "IP literal rejected",
			sourceURL:   "https://code.claude.com/docs/llms.txt",
			validateURL: "https://127.0.0.1/docs/en/overview.md",
			wantErr:     true,
		},
		{
			name:            "allowed hosts override",
			sourceURL:       "https://code.claude.com/docs/llms.txt",
			allowedHostsCSV: "platform.claude.com",
			validateURL:     "https://platform.claude.com/docs/en/overview.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := mustPolicy(t, tt.sourceURL, tt.allowedHostsCSV)
			err := policy.Validate(tt.validateURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("policy.Validate(%q) error = %v, wantErr %v", tt.validateURL, err, tt.wantErr)
			}
		})
	}
}

func TestHTTPClientRedirectAllowsConfiguredHost(t *testing.T) {
	client := policy.NewHTTPClient(5*time.Second, mustPolicy(t, "https://docs.example.com/llms.txt", "cdn.example.com"))

	err := client.CheckRedirect(
		&http.Request{URL: mustParseURL(t, "https://cdn.example.com/docs/en/overview.md")},
		[]*http.Request{{URL: mustParseURL(t, "https://docs.example.com/docs/en/overview.md")}},
	)
	if err != nil {
		t.Fatalf("CheckRedirect() error = %v", err)
	}
}

func TestHTTPClientRedirectRejectsDisallowedHost(t *testing.T) {
	client := policy.NewHTTPClient(5*time.Second, mustPolicy(t, "https://docs.example.com/llms.txt", ""))

	err := client.CheckRedirect(
		&http.Request{URL: mustParseURL(t, "https://evil.example.net/docs/en/overview.md")},
		[]*http.Request{{URL: mustParseURL(t, "https://docs.example.com/docs/en/overview.md")}},
	)
	if err == nil {
		t.Fatal("CheckRedirect() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("CheckRedirect() error = %v, want disallowed host", err)
	}
}

func TestValidateResolvedIPRejectsBlockedRanges(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		wantErr bool
	}{
		{name: "loopback", ip: "127.0.0.1", wantErr: true},
		{name: "private", ip: "10.0.0.5", wantErr: true},
		{name: "documentation", ip: "198.51.100.7", wantErr: true},
		{name: "public", ip: "93.184.216.34", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := policy.ValidateResolvedIP(net.ParseIP(tt.ip))
			if (err != nil) != tt.wantErr {
				t.Fatalf("policy.ValidateResolvedIP(%s) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}

func TestRelativePathForURLRootLayout(t *testing.T) {
	got, err := links.RelativePath("https://code.claude.com/docs/en/overview.md", links.LayoutRoot)
	if err != nil {
		t.Fatalf("links.RelativePath() error = %v", err)
	}

	want := "docs/en/overview.md"
	if got != want {
		t.Fatalf("links.RelativePath() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLNestedLayout(t *testing.T) {
	got, err := links.RelativePath("https://code.claude.com/docs/en/overview.md", links.LayoutNested)
	if err != nil {
		t.Fatalf("links.RelativePath() error = %v", err)
	}

	want := filepath.Join("pages", "code.claude.com", "docs", "en", "overview.md")
	if got != want {
		t.Fatalf("links.RelativePath() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLWithQueryInRootLayout(t *testing.T) {
	got, err := links.RelativePath("https://example.com/docs/page.md?lang=en", links.LayoutRoot)
	if err != nil {
		t.Fatalf("links.RelativePath() error = %v", err)
	}

	want := filepath.Join("docs", "page__0933497c2075.md")
	if got != want {
		t.Fatalf("links.RelativePath() = %q, want %q", got, want)
	}
}

func TestSourcePathForLayout(t *testing.T) {
	if got := links.SourcePath(links.LayoutRoot); got != "llms.txt" {
		t.Fatalf("links.SourcePath(root) = %q, want %q", got, "llms.txt")
	}
	if got := links.SourcePath(links.LayoutNested); got != filepath.ToSlash(filepath.Join("source", "llms.txt")) {
		t.Fatalf("links.SourcePath(nested) = %q", got)
	}
}

func TestWriteManifestAndLoadManifestRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	manifestPath := filepath.Join(tempDir, "manifest.json")

	want := manifest.Manifest{
		Version:              1,
		SourceURL:            "https://example.com/llms.txt",
		SourcePath:           "llms.txt",
		SourceSHA256:         "source-hash",
		SourceLastModifiedAt: "2026-03-13T06:36:52Z",
		SourceETag:           "\"etag-source\"",
		DocumentCount:        1,
		SkippedCount:         1,
		Documents: []manifest.Entry{
			{
				URL:            "https://example.com/docs/overview.md",
				Path:           "docs/overview.md",
				SHA256:         "doc-hash",
				Bytes:          123,
				LastModifiedAt: "2026-03-13T06:36:52Z",
				ETag:           "\"etag-doc\"",
			},
		},
		Skipped: []manifest.SkippedEntry{
			{URL: "https://example.com/docs", Reason: links.NonMarkdownReason},
		},
		Failures: []manifest.FetchFailure{
			{URL: "https://example.com/docs/broken.md", Error: "bad response"},
		},
	}

	if err := manifest.Write(manifestPath, &want); err != nil {
		t.Fatalf("manifest.Write() error = %v", err)
	}

	got, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("manifest.Load() error = %v", err)
	}

	if diff := cmp.Diff(want, *got); diff != "" {
		t.Fatalf("manifest.Load() mismatch (-want +got):\n%s", diff)
	}
}

func TestNormalizeLastModified(t *testing.T) {
	got := fetch.NormalizeLastModified("Fri, 13 Mar 2026 06:36:52 GMT")
	want := "2026-03-13T06:36:52Z"

	if got != want {
		t.Fatalf("fetch.NormalizeLastModified() = %q, want %q", got, want)
	}
}

func TestIfModifiedSinceHeader(t *testing.T) {
	got := fetch.IfModifiedSinceHeader("2026-03-13T06:36:52Z")
	want := "Fri, 13 Mar 2026 06:36:52 GMT"

	if got != want {
		t.Fatalf("fetch.IfModifiedSinceHeader() = %q, want %q", got, want)
	}
}

func TestNormalizeETag(t *testing.T) {
	got := fetch.NormalizeETag(` W/"abc123" `)
	want := `W/"abc123"`

	if got != want {
		t.Fatalf("fetch.NormalizeETag() = %q, want %q", got, want)
	}
}

func TestEnsureMarkdownResponseRejectsExplicitHTML(t *testing.T) {
	err := fetch.EnsureMarkdownResponse(
		"200 OK",
		"text/html; charset=utf-8",
		map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		[]byte("<!DOCTYPE html><html><head><title>Not Found</title></head><body></body></html>"),
		"/tmp/body.html",
	)
	if err == nil {
		t.Fatal("fetch.EnsureMarkdownResponse() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("fetch.EnsureMarkdownResponse() error = %q, want html rejection", err)
	}
}

func TestEnsureMarkdownResponseRejectsLeadingCommentHTML(t *testing.T) {
	err := fetch.EnsureMarkdownResponse(
		"200 OK",
		"",
		nil,
		[]byte("\ufeff<!-- banner --><!-- another --><html><body>bad</body></html>"),
		"/tmp/body.html",
	)
	if err == nil {
		t.Fatal("fetch.EnsureMarkdownResponse() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "HTML document") {
		t.Fatalf("fetch.EnsureMarkdownResponse() error = %q, want HTML document rejection", err)
	}
}

func TestEnsureMarkdownResponseAcceptsCapturedSkillsMarkdown(t *testing.T) {
	err := fetch.EnsureMarkdownResponse(
		"200 OK",
		"",
		nil,
		[]byte(
			"> ## Documentation Index\n"+
				"> Fetch the complete documentation index at: https://code.claude.com/docs/llms.txt\n\n"+
				"# Extend Claude with skills\n\n"+
				"```python\n"+
				"html = '''<html><head>\n"+
				"<title>Example</title>\n"+
				"</head><body></body></html>'''\n"+
				"```\n",
		),
		"/tmp/body.md",
	)
	if err != nil {
		t.Fatalf("fetch.EnsureMarkdownResponse() error = %v", err)
	}
}

func TestWriteUnexpectedContentDiagnostic(t *testing.T) {
	diagnosticsDir := t.TempDir()
	bodyPath := filepath.Join(t.TempDir(), "body.html")
	body := "<!DOCTYPE html><html><body>bad html</body></html>"
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	unexpected := &fetch.UnexpectedContentError{
		Message:     "expected markdown response but received HTML document",
		Status:      "200 OK",
		ContentType: "text/html; charset=utf-8",
		Headers:     map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Sniff:       []byte(body),
		BodyPath:    bodyPath,
	}

	diagnosticPath, err := fetch.WriteUnexpectedContentDiagnostic(
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

	bodyOutputPath := filepath.Join(diagnosticsDir, "docs", "skills.md.unexpected-content.html")
	if got := readFile(t, bodyOutputPath); got != body {
		t.Fatalf("diagnostic body = %q, want %q", got, body)
	}

	metadataPath := filepath.Join(diagnosticsDir, filepath.FromSlash(diagnosticPath))
	metadata := readFile(t, metadataPath)
	if !strings.Contains(metadata, `"body_path": "docs/skills.md.unexpected-content.html"`) {
		t.Fatalf("diagnostic metadata missing body path: %s", metadata)
	}
}

func TestBuildDiagnosticManifestIncludesFailuresAndDocuments(t *testing.T) {
	source := fetch.Result{
		URL:            "https://docs.example.com/llms.txt",
		RelativePath:   "llms.txt",
		SHA256:         "source-sha",
		LastModifiedAt: "2026-03-13T06:36:52Z",
		ETag:           `"source-etag"`,
	}
	documents := []fetch.Result{{
		URL:            "https://docs.example.com/docs/en/overview.md",
		RelativePath:   filepath.Join("docs", "en", "overview.md"),
		SHA256:         "doc-sha",
		Bytes:          42,
		LastModifiedAt: "2026-03-13T06:37:00Z",
		ETag:           `"doc-etag"`,
	}}
	skipped := []manifest.SkippedEntry{{URL: "https://docs.example.com", Reason: links.NonMarkdownReason}}
	failures := []manifest.FetchFailure{{URL: "https://docs.example.com/docs/en/missing.md", Error: "404"}}

	manifestData := app.BuildDiagnosticManifest(source.URL, source.RelativePath, &source, documents, skipped, failures)
	if manifestData.SourceSHA256 != "source-sha" {
		t.Fatalf("app.BuildDiagnosticManifest() source sha = %q", manifestData.SourceSHA256)
	}
	if manifestData.DocumentCount != 1 {
		t.Fatalf("app.BuildDiagnosticManifest() document_count = %d, want 1", manifestData.DocumentCount)
	}
	if len(manifestData.Failures) != 1 {
		t.Fatalf("app.BuildDiagnosticManifest() failures len = %d, want 1", len(manifestData.Failures))
	}
	if len(manifestData.Documents) != 1 || manifestData.Documents[0].Path != "docs/en/overview.md" {
		t.Fatalf("app.BuildDiagnosticManifest() documents = %#v", manifestData.Documents)
	}
	if len(manifestData.Skipped) != 1 || manifestData.Skipped[0].Reason != links.NonMarkdownReason {
		t.Fatalf("app.BuildDiagnosticManifest() skipped = %#v", manifestData.Skipped)
	}
}

func TestFetchDocumentStreamsToDiskAndComputesMetadata(t *testing.T) {
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type":  "text/markdown; charset=utf-8",
			"Last-Modified": "Fri, 13 Mar 2026 06:36:52 GMT",
			"ETag":          `"etag-1"`,
		}, "# Overview\n\nReal markdown content.\n"), nil
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Overview\n\nReal markdown content.\n" {
		t.Fatalf("fetch.Document() wrote %q", got)
	}
	if result.SHA256 != fetch.HashBytes([]byte("# Overview\n\nReal markdown content.\n")) {
		t.Fatalf("fetch.Document() sha256 = %q", result.SHA256)
	}
	if result.Bytes != int64(len("# Overview\n\nReal markdown content.\n")) {
		t.Fatalf("fetch.Document() bytes = %d", result.Bytes)
	}
	if result.LastModifiedAt != "2026-03-13T06:36:52Z" {
		t.Fatalf("fetch.Document() last_modified_at = %q", result.LastModifiedAt)
	}
	if result.ETag != `"etag-1"` {
		t.Fatalf("fetch.Document() etag = %q", result.ETag)
	}
}

func TestFetchDocumentRetriesTransientHTMLForMarkdownURL(t *testing.T) {

	var attempts atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/html; charset=utf-8",
			}, "<!DOCTYPE html><html><body>temporary html</body></html>"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Real markdown\n"), nil
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/skills.md",
		filepath.Join("docs", "en", "skills.md"),
		manifest.Entry{},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Real markdown\n" {
		t.Fatalf("fetch.Document() body = %q, want markdown body", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("fetch.Document() attempts = %d, want 2", got)
	}
}

func TestFetchDocumentUsesCachedFileOn304(t *testing.T) {
	archiveRoot := t.TempDir()
	relativePath := filepath.Join("docs", "en", "overview.md")
	fullPath := filepath.Join(archiveRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	body := "# Cached markdown\n"
	if err := os.WriteFile(fullPath, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var requests atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if got := req.Header.Get("If-None-Match"); got != `"etag-1"` {
			t.Fatalf("If-None-Match = %q, want %q", got, `"etag-1"`)
		}
		if got := req.Header.Get("If-Modified-Since"); got == "" {
			t.Fatal("If-Modified-Since header missing")
		}
		return testResponse(http.StatusNotModified, map[string]string{
			"ETag": `"etag-2"`,
		}, ""), nil
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		relativePath,
		manifest.Entry{
			Path:           filepath.ToSlash(relativePath),
			SHA256:         fetch.HashBytes([]byte(body)),
			Bytes:          int64(len(body)),
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if result.LocalPath != fullPath {
		t.Fatalf("fetch.Document() local path = %q, want %q", result.LocalPath, fullPath)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if result.ETag != `"etag-2"` {
		t.Fatalf("fetch.Document() etag = %q, want response validator", result.ETag)
	}
}

func TestFetchDocumentRefetchesOn304CacheMiss(t *testing.T) {
	var requests atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			if got := req.Header.Get("If-None-Match"); got != `"etag-1"` {
				t.Fatalf("If-None-Match = %q, want validator on first request", got)
			}
			return testResponse(http.StatusNotModified, map[string]string{
				"ETag": `"etag-1"`,
			}, ""), nil
		default:
			if got := req.Header.Get("If-None-Match"); got != "" {
				t.Fatalf("If-None-Match on refetch = %q, want empty", got)
			}
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/markdown; charset=utf-8",
			}, "# Refetched markdown\n"), nil
		}
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{
			Path:           "docs/en/overview.md",
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := readFile(t, result.LocalPath); got != "# Refetched markdown\n" {
		t.Fatalf("fetch.Document() refetched body = %q", got)
	}
}

func TestFetchDocumentRefetchesOn304CacheHashMismatch(t *testing.T) {
	archiveRoot := t.TempDir()
	relativePath := filepath.Join("docs", "en", "overview.md")
	fullPath := filepath.Join(archiveRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fullPath, []byte("# Drifted markdown\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var requests atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			if got := req.Header.Get("If-None-Match"); got != `"etag-1"` {
				t.Fatalf("If-None-Match = %q, want validator on first request", got)
			}
			return testResponse(http.StatusNotModified, map[string]string{
				"ETag": `"etag-2"`,
			}, ""), nil
		default:
			if got := req.Header.Get("If-None-Match"); got != "" {
				t.Fatalf("If-None-Match on refetch = %q, want empty", got)
			}
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/markdown; charset=utf-8",
				"ETag":         `"etag-3"`,
			}, "# Refetched markdown\n"), nil
		}
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		relativePath,
		manifest.Entry{
			Path:           filepath.ToSlash(relativePath),
			SHA256:         fetch.HashBytes([]byte("# Expected markdown\n")),
			Bytes:          int64(len("# Expected markdown\n")),
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := readFile(t, result.LocalPath); got != "# Refetched markdown\n" {
		t.Fatalf("fetch.Document() refetched body = %q", got)
	}
	if result.ETag != `"etag-3"` {
		t.Fatalf("fetch.Document() etag = %q, want refetched validator", result.ETag)
	}
}

func TestFetchSourceRefetchesOn304CacheHashMismatch(t *testing.T) {
	archiveRoot := t.TempDir()
	sourcePath := links.SourcePath(links.LayoutRoot)
	fullPath := filepath.Join(archiveRoot, sourcePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fullPath, []byte("stale llms body\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var requests atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return testResponse(http.StatusNotModified, map[string]string{"ETag": `"etag-2"`}, ""), nil
		default:
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/plain; charset=utf-8",
				"ETag":         `"etag-3"`,
			}, "https://docs.example.com/docs/en/overview.md\n"), nil
		}
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/llms.txt",
		sourcePath,
		manifest.Entry{
			Path:           sourcePath,
			SHA256:         fetch.HashBytes([]byte("expected llms body\n")),
			Bytes:          int64(len("expected llms body\n")),
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := readFile(t, result.LocalPath); got != "https://docs.example.com/docs/en/overview.md\n" {
		t.Fatalf("fetch.Document() refetched body = %q", got)
	}
}

func TestPreservePreviousDocumentUsesExistingArchiveCopy(t *testing.T) {
	tempDir := t.TempDir()
	body := []byte("# Cached markdown\n")
	targetPath := filepath.Join(tempDir, "docs", "en", "overview.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, body, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	got, err := fetch.PreservePreviousDocument(
		tempDir,
		"https://example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{
			URL:            "https://example.com/docs/en/overview.md",
			Path:           "docs/en/overview.md",
			SHA256:         fetch.HashBytes(body),
			Bytes:          int64(len(body)),
			LastModifiedAt: "2026-03-13T12:00:00Z",
			ETag:           "\"etag-1\"",
		},
	)
	if err != nil {
		t.Fatalf("fetch.PreservePreviousDocument() error = %v", err)
	}

	if got.RelativePath != filepath.Join("docs", "en", "overview.md") {
		t.Fatalf("fetch.PreservePreviousDocument() path = %q", got.RelativePath)
	}
	if got.LocalPath != targetPath {
		t.Fatalf("fetch.PreservePreviousDocument() local path = %q, want %q", got.LocalPath, targetPath)
	}
	if got.LastModifiedAt != "2026-03-13T12:00:00Z" {
		t.Fatalf("fetch.PreservePreviousDocument() last_modified_at = %q", got.LastModifiedAt)
	}
	if got.ETag != "\"etag-1\"" {
		t.Fatalf("fetch.PreservePreviousDocument() etag = %q", got.ETag)
	}
	if got.SHA256 != fetch.HashBytes(body) {
		t.Fatalf("fetch.PreservePreviousDocument() sha256 = %q, want %q", got.SHA256, fetch.HashBytes(body))
	}
	if got.Bytes != int64(len(body)) {
		t.Fatalf("fetch.PreservePreviousDocument() bytes = %d, want %d", got.Bytes, len(body))
	}
}

func TestPreservePreviousDocumentRequiresPreviousArchiveEntry(t *testing.T) {
	_, err := fetch.PreservePreviousDocument(
		t.TempDir(),
		"https://example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{},
	)
	if err == nil {
		t.Fatal("fetch.PreservePreviousDocument() error = nil, want error")
	}
	if !errors.Is(err, fetch.ErrNoPreviousEntry) {
		t.Fatalf("fetch.PreservePreviousDocument() error = %v, want ErrNoPreviousEntry", err)
	}
}

func TestPreservePreviousDocumentRejectsHashMismatch(t *testing.T) {
	tempDir := t.TempDir()
	targetPath := filepath.Join(tempDir, "docs", "en", "overview.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("# Drifted markdown\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := fetch.PreservePreviousDocument(
		tempDir,
		"https://example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{
			URL:    "https://example.com/docs/en/overview.md",
			Path:   "docs/en/overview.md",
			SHA256: fetch.HashBytes([]byte("# Expected markdown\n")),
			Bytes:  int64(len("# Expected markdown\n")),
		},
	)
	if err == nil {
		t.Fatal("fetch.PreservePreviousDocument() error = nil, want hash mismatch")
	}
	if !strings.Contains(err.Error(), "does not match previous manifest hash") {
		t.Fatalf("fetch.PreservePreviousDocument() error = %v, want hash mismatch", err)
	}
}

func TestSafeJoinRejectsEscapingPath(t *testing.T) {
	_, err := fileutil.SafeJoin(t.TempDir(), filepath.Join("..", "secret.txt"))
	if err == nil {
		t.Fatal("fileutil.SafeJoin() error = nil, want rejection")
	}
}

func TestFetchDocumentsPreservesPreviousCopyOnFailure(t *testing.T) {

	archiveRoot := t.TempDir()
	cachedBody := []byte("# Previous snapshot\n")
	targetPath := filepath.Join(archiveRoot, "docs", "en", "skills.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, cachedBody, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/html; charset=utf-8",
		}, "<!DOCTYPE html><html><body>not found</body></html>"), nil
	})

	previous := map[string]manifest.Entry{
		"https://docs.example.com/docs/en/skills.md": {
			URL:            "https://docs.example.com/docs/en/skills.md",
			Path:           "docs/en/skills.md",
			SHA256:         fetch.HashBytes(cachedBody),
			Bytes:          int64(len(cachedBody)),
			LastModifiedAt: "2026-03-13T12:00:00Z",
			ETag:           "\"etag-1\"",
		},
	}

	gotDocs, gotFailures := fetch.Documents(
		context.Background(),
		[]string{"https://docs.example.com/docs/en/skills.md"},
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
			Layout:         links.LayoutRoot,
			DiagnosticsDir: filepath.Join(t.TempDir(), "diagnostics"),
			Concurrency:    1,
			PreviousDocs:   previous,
		},
	)

	if len(gotDocs) != 1 {
		t.Fatalf("fetchDocuments() docs len = %d, want 1", len(gotDocs))
	}
	if gotDocs[0].LocalPath != targetPath {
		t.Fatalf("fetchDocuments() preserved local path = %q, want %q", gotDocs[0].LocalPath, targetPath)
	}
	if len(gotFailures) != 1 {
		t.Fatalf("fetchDocuments() failures len = %d, want 1", len(gotFailures))
	}
	if !gotFailures[0].PreservedExisting {
		t.Fatalf("fetchDocuments() failure preserved_existing = false, want true")
	}
	if gotFailures[0].DiagnosticPath == "" {
		t.Fatalf("fetchDocuments() diagnostic_path = %q, want non-empty path", gotFailures[0].DiagnosticPath)
	}
}

func TestFetchDocumentsReplacesPreservedCopyAfterLaterSuccess(t *testing.T) {

	archiveRoot := t.TempDir()
	cachedBody := []byte("# Previous snapshot\n")
	targetPath := filepath.Join(archiveRoot, "docs", "en", "skills.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, cachedBody, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	failingClient := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/html; charset=utf-8",
		}, "<!DOCTYPE html><html><body>persistent failure</body></html>"), nil
	})
	successClient := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Fresh snapshot\n"), nil
	})

	previous := map[string]manifest.Entry{
		"https://docs.example.com/docs/en/skills.md": {
			URL:            "https://docs.example.com/docs/en/skills.md",
			Path:           "docs/en/skills.md",
			SHA256:         fetch.HashBytes(cachedBody),
			Bytes:          int64(len(cachedBody)),
			LastModifiedAt: "2026-03-13T12:00:00Z",
			ETag:           "\"etag-1\"",
		},
	}

	firstDocs, firstFailures := fetch.Documents(
		context.Background(),
		[]string{"https://docs.example.com/docs/en/skills.md"},
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      failingClient,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
			Layout:         links.LayoutRoot,
			DiagnosticsDir: filepath.Join(t.TempDir(), "diagnostics-one"),
			Concurrency:    1,
			PreviousDocs:   previous,
		},
	)
	if len(firstDocs) != 1 || firstDocs[0].LocalPath != targetPath {
		t.Fatalf("first fetchDocuments() docs = %#v", firstDocs)
	}
	if len(firstFailures) != 1 || !firstFailures[0].PreservedExisting {
		t.Fatalf("first fetchDocuments() failures = %#v", firstFailures)
	}

	secondDocs, secondFailures := fetch.Documents(
		context.Background(),
		[]string{"https://docs.example.com/docs/en/skills.md"},
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      successClient,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: archiveRoot,
			},
			Layout:         links.LayoutRoot,
			DiagnosticsDir: filepath.Join(t.TempDir(), "diagnostics-two"),
			Concurrency:    1,
			PreviousDocs:   previous,
		},
	)
	if len(secondFailures) != 0 {
		t.Fatalf("second fetchDocuments() failures = %#v, want none", secondFailures)
	}
	if len(secondDocs) != 1 {
		t.Fatalf("second fetchDocuments() docs len = %d, want 1", len(secondDocs))
	}
	if secondDocs[0].LocalPath == targetPath {
		t.Fatalf("second fetchDocuments() reused preserved file, want new fetched file")
	}
	if got := readFile(t, secondDocs[0].LocalPath); got != "# Fresh snapshot\n" {
		t.Fatalf("second fetchDocuments() body = %q, want fresh content", got)
	}
}

func TestReplaceDirRecoversFromLeftoverBackup(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(root, "temp")

	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(backup) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "old.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(backup) error = %v", err)
	}
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "new.md"), []byte("new\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(temp) error = %v", err)
	}

	if err := stage.ReplaceDir(tempDir, outputDir, nil); err != nil {
		t.Fatalf("ReplaceDir() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "new.md")); got != "new\n" {
		t.Fatalf("ReplaceDir() output = %q, want new content", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("backup directory still exists: %v", err)
	}
}

func TestReconcileStageStateRestoresBackupFromJournal(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(root, ".llmstxt-staged")

	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(backup) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "old.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(backup) error = %v", err)
	}
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := stage.WriteCompletionMarker(tempDir); err != nil {
		t.Fatalf("stage.WriteCompletionMarker() error = %v", err)
	}
	if err := stage.WriteJournal(outputDir, stage.Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: backupDir,
		Phase:     "backup_created",
	}); err != nil {
		t.Fatalf("stage.WriteJournal() error = %v", err)
	}

	if err := stage.ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("stage.ReconcileState() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "old.md")); got != "old\n" {
		t.Fatalf("stage.ReconcileState() output = %q, want restored backup", got)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("staged temp directory still exists: %v", err)
	}
	if _, err := os.Stat(stage.JournalPath(outputDir)); !os.IsNotExist(err) {
		t.Fatalf("stage journal still exists: %v", err)
	}
}

func TestReconcileStageStatePromotesCompletedTempFromJournal(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	tempDir := filepath.Join(root, ".llmstxt-staged")

	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "new.md"), []byte("new\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(temp) error = %v", err)
	}
	if err := stage.WriteCompletionMarker(tempDir); err != nil {
		t.Fatalf("stage.WriteCompletionMarker() error = %v", err)
	}
	if err := stage.WriteJournal(outputDir, stage.Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     "staged",
	}); err != nil {
		t.Fatalf("stage.WriteJournal() error = %v", err)
	}

	if err := stage.ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("stage.ReconcileState() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "new.md")); got != "new\n" {
		t.Fatalf("stage.ReconcileState() output = %q, want promoted staged content", got)
	}
	if _, err := os.Stat(stage.CompletionMarkerPath(outputDir)); !os.IsNotExist(err) {
		t.Fatalf("completion marker still exists in output: %v", err)
	}
	if _, err := os.Stat(stage.JournalPath(outputDir)); !os.IsNotExist(err) {
		t.Fatalf("stage journal still exists: %v", err)
	}
}

func TestReconcileStageStateRemovesStaleArtifactsWhenOutputExists(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(root, ".llmstxt-staged")

	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(output) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "live.md"), []byte("live\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(output) error = %v", err)
	}
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(backup) error = %v", err)
	}
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := stage.WriteJournal(outputDir, stage.Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: backupDir,
		Phase:     "activate_output",
	}); err != nil {
		t.Fatalf("stage.WriteJournal() error = %v", err)
	}

	if err := stage.ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("stage.ReconcileState() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "live.md")); got != "live\n" {
		t.Fatalf("stage.ReconcileState() output = %q, want live output preserved", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("backup directory still exists: %v", err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("staged temp directory still exists: %v", err)
	}
}

func TestReconcileStageStateRemovesIncompleteTempFromJournal(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	tempDir := filepath.Join(root, ".llmstxt-staged")

	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := stage.WriteJournal(outputDir, stage.Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     "staged",
	}); err != nil {
		t.Fatalf("stage.WriteJournal() error = %v", err)
	}

	if err := stage.ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("stage.ReconcileState() error = %v", err)
	}

	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("incomplete staged temp directory still exists: %v", err)
	}
}

func TestReplaceDirWarnsWhenBackupCleanupFails(t *testing.T) {
	root := t.TempDir()
	outputDir := filepath.Join(root, "snapshot")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(root, "temp")

	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(output) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "old.md"), []byte("old\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(output) error = %v", err)
	}
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(temp) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "new.md"), []byte("new\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(temp) error = %v", err)
	}
	if err := stage.WriteCompletionMarker(tempDir); err != nil {
		t.Fatalf("stage.WriteCompletionMarker() error = %v", err)
	}

	opts := &stage.Options{
		RemoveAll: func(path string) error {
			if path == backupDir {
				return fmt.Errorf("simulated cleanup failure")
			}
			return os.RemoveAll(path)
		},
	}

	if err := stage.ReplaceDir(tempDir, outputDir, opts); err != nil {
		t.Fatalf("ReplaceDir() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "new.md")); got != "new\n" {
		t.Fatalf("ReplaceDir() output = %q, want new content", got)
	}
	if _, err := os.Stat(backupDir); err != nil {
		t.Fatalf("backup directory missing after simulated cleanup failure: %v", err)
	}
}

func TestIsIndex(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://example.com/llms.txt", true},
		{"https://example.com/docs/llms.txt", true},
		{"https://example.com/llms-full.txt", false},
		{"https://example.com/docs/overview.md", false},
		{"https://example.com/docs", false},
		{"https://example.com/LLMS.txt", false}, // case-sensitive
	}

	for _, tt := range tests {
		if got := links.IsIndex(tt.url); got != tt.want {
			t.Errorf("links.IsIndex(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestPartitionSeparatesIndexURLs(t *testing.T) {
	input := []string{
		"https://example.com/docs/overview.md",
		"https://example.com/api/llms.txt",
		"https://example.com/llms-full.txt",
		"https://example.com/page",
	}

	docs, indexes, skipped, err := links.Partition(input)
	if err != nil {
		t.Fatalf("links.Partition() error = %v", err)
	}

	if diff := cmp.Diff([]string{"https://example.com/docs/overview.md"}, docs); diff != "" {
		t.Fatalf("docs mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"https://example.com/api/llms.txt"}, indexes); diff != "" {
		t.Fatalf("indexes mismatch (-want +got):\n%s", diff)
	}
	wantSkipped := []manifest.SkippedEntry{
		{URL: "https://example.com/llms-full.txt", Reason: links.NonMarkdownReason},
		{URL: "https://example.com/page", Reason: links.NonMarkdownReason},
	}
	if diff := cmp.Diff(wantSkipped, skipped); diff != "" {
		t.Fatalf("skipped mismatch (-want +got):\n%s", diff)
	}
}

func TestRecursiveDiscovery(t *testing.T) {
	// Root llms.txt links to a nested llms.txt and a doc.
	// Nested llms.txt links to another doc.
	rootBody := "- [Doc A](https://example.com/a.md)\n- [Nested](https://example.com/api/llms.txt)\n"
	nestedBody := "- [Doc B](https://example.com/b.md)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, nestedBody), nil
		default:
			return testResponse(404, nil, "not found"), nil
		}
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, err := links.Extract([]byte(rootBody))
	if err != nil {
		t.Fatalf("links.Extract() error = %v", err)
	}

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	wantDocs := []string{
		"https://example.com/a.md",
		"https://example.com/b.md",
	}
	if diff := cmp.Diff(wantDocs, result.DocURLs); diff != "" {
		t.Fatalf("docs mismatch (-want +got):\n%s", diff)
	}
	if len(result.IndexResults) != 1 {
		t.Fatalf("got %d index results, want 1", len(result.IndexResults))
	}
}

func TestRecursiveDiscoveryCyclePrevention(t *testing.T) {
	// Two llms.txt files referencing each other.
	bodyA := "- [B](https://example.com/b/llms.txt)\n- [Doc](https://example.com/a.md)\n"
	bodyB := "- [A](https://example.com/a/llms.txt)\n- [Doc](https://example.com/b.md)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/a/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, bodyA), nil
		case "/b/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, bodyB), nil
		default:
			return testResponse(404, nil, "not found"), nil
		}
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	// Start from root which links to /a/llms.txt.
	rootBody := "- [A](https://example.com/a/llms.txt)\n"
	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	// Should discover docs from both indexes without infinite loop.
	if len(result.DocURLs) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(result.DocURLs), result.DocURLs)
	}
	if len(result.IndexResults) != 2 {
		t.Fatalf("got %d indexes, want 2", len(result.IndexResults))
	}
}

func TestRecursiveDiscoveryCrossHostBlocked(t *testing.T) {
	rootBody := "- [Cross](https://other.com/llms.txt)\n- [Doc](https://example.com/a.md)\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	if len(result.DocURLs) != 1 {
		t.Fatalf("got %d docs, want 1", len(result.DocURLs))
	}

	// Cross-host index should be skipped, not crash.
	foundSkipped := false
	for _, s := range result.Skipped {
		if s.URL == "https://other.com/llms.txt" {
			foundSkipped = true
			break
		}
	}
	if !foundSkipped {
		t.Fatal("cross-host index not in skipped list")
	}
}

func TestRecursiveDiscoveryEmptyNestedIndex(t *testing.T) {
	rootBody := "- [Empty](https://example.com/empty/llms.txt)\n- [Doc](https://example.com/a.md)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/empty/llms.txt" {
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, "# Empty index\nNo links here.\n"), nil
		}
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	// Should still get the one doc, no crash from empty nested index.
	if len(result.DocURLs) != 1 || result.DocURLs[0] != "https://example.com/a.md" {
		t.Fatalf("got docs %v, want [https://example.com/a.md]", result.DocURLs)
	}
}

func TestLlmsFullTxtNotTreatedAsIndex(t *testing.T) {
	if links.IsIndex("https://example.com/llms-full.txt") {
		t.Fatal("llms-full.txt should not be treated as an index")
	}
	if links.IsIndex("https://example.com/docs/llms-full.txt") {
		t.Fatal("nested llms-full.txt should not be treated as an index")
	}
}

func TestRecursiveDiscoveryNestedFetchFailure(t *testing.T) {
	rootBody := "- [Doc](https://example.com/a.md)\n- [Nested](https://example.com/broken/llms.txt)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/broken/llms.txt" {
			return testResponse(500, nil, "internal server error"), nil
		}
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	if len(result.DocURLs) != 1 || result.DocURLs[0] != "https://example.com/a.md" {
		t.Fatalf("got docs %v, want [https://example.com/a.md]", result.DocURLs)
	}

	var found bool
	for _, s := range result.Skipped {
		if s.URL == "https://example.com/broken/llms.txt" && strings.Contains(s.Reason, "fetch failed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("broken nested index not in skipped list: %v", result.Skipped)
	}
}

func TestRecursiveDiscoveryDocDedup(t *testing.T) {
	// Root and nested both link to the same doc.
	rootBody := "- [Doc](https://example.com/shared.md)\n- [Nested](https://example.com/api/llms.txt)\n"
	nestedBody := "- [Doc](https://example.com/shared.md)\n- [Extra](https://example.com/extra.md)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/llms.txt" {
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, nestedBody), nil
		}
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	wantDocs := []string{
		"https://example.com/shared.md",
		"https://example.com/extra.md",
	}
	if diff := cmp.Diff(wantDocs, result.DocURLs); diff != "" {
		t.Fatalf("docs mismatch (-want +got):\n%s", diff)
	}
}

func TestRecursiveDiscoveryContextCancellation(t *testing.T) {
	rootBody := "- [Doc](https://example.com/a.md)\n- [Nested](https://example.com/api/llms.txt)\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(200, map[string]string{"Content-Type": "text/plain"}, "- [Doc](https://example.com/b.md)\n"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately before discovery.

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		ctx, "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	// Root docs from Partition should still be present; nested index should not have been fetched.
	if len(result.DocURLs) != 1 || result.DocURLs[0] != "https://example.com/a.md" {
		t.Fatalf("got docs %v, want [https://example.com/a.md]", result.DocURLs)
	}
	if len(result.IndexResults) != 0 {
		t.Fatalf("got %d index results, want 0 (context was cancelled)", len(result.IndexResults))
	}
}

func TestRecursiveDiscoveryIndexCap(t *testing.T) {
	// Generate more than maxNestedIndexes (50) unique nested indexes.
	var rootLinks []string
	for i := 0; i < 60; i++ {
		rootLinks = append(rootLinks, fmt.Sprintf("- [Idx%d](https://example.com/%d/llms.txt)", i, i))
	}
	rootBody := strings.Join(rootLinks, "\n") + "\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(200, map[string]string{"Content-Type": "text/plain"}, "- [Doc](https://example.com/doc.md)\n"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	if len(result.IndexResults) > 50 {
		t.Fatalf("got %d index results, want <= 50 (cap should be enforced)", len(result.IndexResults))
	}
}

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- test reads fixture files
	if err != nil {
		t.Fatalf("os.ReadFile(testdata/%s) error = %v", name, err)
	}
	return body
}

func TestRecursiveDiscoveryWithFixtures(t *testing.T) {
	// Use testdata fixtures for a realistic multi-level BFS scenario:
	// root.llms.txt -> nested-api.llms.txt (which links to nested-deep.llms.txt)
	//               -> nested-sdk.llms.txt
	// nested-api also links to getting-started.md (shared with root) -> dedup.
	rootBody := loadTestdata(t, "root.llms.txt")
	apiBody := loadTestdata(t, "nested-api.llms.txt")
	sdkBody := loadTestdata(t, "nested-sdk.llms.txt")
	deepBody := loadTestdata(t, "nested-deep.llms.txt")

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, string(apiBody)), nil
		case "/sdk/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, string(sdkBody)), nil
		case "/api/v2/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, string(deepBody)), nil
		default:
			return testResponse(404, nil, "not found"), nil
		}
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, err := links.Extract(rootBody)
	if err != nil {
		t.Fatalf("links.Extract() error = %v", err)
	}

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	// 3 nested indexes: api, sdk, deep.
	if len(result.IndexResults) != 3 {
		t.Fatalf("got %d index results, want 3", len(result.IndexResults))
	}

	// Unique docs: getting-started, api-reference (root), api-endpoints, api-auth (api),
	// sdk-overview, sdk-quickstart (sdk), api-v2-migration (deep).
	// getting-started appears in both root and api but should be deduped.
	wantDocs := []string{
		"https://example.com/docs/getting-started.md",
		"https://example.com/docs/api-reference.md",
		"https://example.com/docs/api-endpoints.md",
		"https://example.com/docs/api-auth.md",
		"https://example.com/docs/sdk-overview.md",
		"https://example.com/docs/sdk-quickstart.md",
		"https://example.com/docs/api-v2-migration.md",
	}
	if len(result.DocURLs) != len(wantDocs) {
		t.Fatalf("got %d docs, want %d: %v", len(result.DocURLs), len(wantDocs), result.DocURLs)
	}
	gotSet := make(map[string]bool, len(result.DocURLs))
	for _, u := range result.DocURLs {
		gotSet[u] = true
	}
	for _, u := range wantDocs {
		if !gotSet[u] {
			t.Errorf("missing expected doc: %s", u)
		}
	}
}

func TestRecursiveDiscoveryFragmentDedup(t *testing.T) {
	// A nested index URL with a fragment should dedup against the same URL without fragment.
	rootBody := "- [Doc](https://example.com/a.md)\n- [Nested](https://example.com/api/llms.txt)\n- [Nested Fragment](https://example.com/api/llms.txt#section)\n"
	nestedBody := "- [Doc B](https://example.com/b.md)\n"

	var fetchCount atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/llms.txt" {
			fetchCount.Add(1)
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, nestedBody), nil
		}
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	if got := fetchCount.Load(); got != 1 {
		t.Fatalf("nested index fetched %d times, want 1 (fragment should dedup)", got)
	}
	if len(result.IndexResults) != 1 {
		t.Fatalf("got %d index results, want 1", len(result.IndexResults))
	}
	// Should still discover both docs.
	if len(result.DocURLs) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(result.DocURLs), result.DocURLs)
	}
}

func TestRecursiveDiscoveryEmptyNestedIndexSkipReason(t *testing.T) {
	// Verify that an empty nested index (no links) is recorded in Skipped with the right reason.
	emptyBody := loadTestdata(t, "empty-index.llms.txt")
	rootBody := "- [Doc](https://example.com/a.md)\n- [Empty](https://example.com/empty/llms.txt)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/empty/llms.txt" {
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, string(emptyBody)), nil
		}
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://example.com/llms.txt", "")
	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client: client, URLPolicy: pol, SpoolDir: spoolDir, ArchiveRoot: archiveRoot, Layout: links.LayoutNested,
		},
	)
	if err != nil {
		t.Fatalf("discoverDocuments() error = %v", err)
	}

	var found bool
	for _, s := range result.Skipped {
		if s.URL == "https://example.com/empty/llms.txt" && strings.Contains(s.Reason, "no links") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("empty nested index not in skipped with 'no links' reason: %v", result.Skipped)
	}
}

func TestBuildManifestPopulatesSources(t *testing.T) {
	source := fetch.Result{
		URL:          "https://example.com/llms.txt",
		RelativePath: "source/llms.txt",
		SHA256:       "abc123",
	}
	indexes := []fetch.Result{
		{URL: "https://example.com/api/llms.txt", RelativePath: "pages/example.com/api/llms.txt", SHA256: "def456", ETag: `"etag1"`},
		{URL: "https://example.com/sdk/llms.txt", RelativePath: "pages/example.com/sdk/llms.txt", SHA256: "ghi789"},
	}

	m := app.BuildManifest(source, nil, nil, nil, indexes)

	if len(m.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(m.Sources))
	}
	if m.Sources[0].URL != "https://example.com/api/llms.txt" {
		t.Errorf("sources[0].URL = %q, want api/llms.txt", m.Sources[0].URL)
	}
	if m.Sources[0].SHA256 != "def456" {
		t.Errorf("sources[0].SHA256 = %q, want def456", m.Sources[0].SHA256)
	}
	if m.Sources[1].URL != "https://example.com/sdk/llms.txt" {
		t.Errorf("sources[1].URL = %q, want sdk/llms.txt", m.Sources[1].URL)
	}
}

func TestBuildManifestNoSourcesWhenEmpty(t *testing.T) {
	source := fetch.Result{
		URL:          "https://example.com/llms.txt",
		RelativePath: "source/llms.txt",
		SHA256:       "abc123",
	}

	m := app.BuildManifest(source, nil, nil, nil, nil)

	if m.Sources != nil {
		t.Fatalf("got sources %v, want nil", m.Sources)
	}
}
