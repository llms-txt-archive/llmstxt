package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
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

func mustPolicy(t *testing.T, sourceURL string, allowedHostsCSV string) *urlPolicy {
	t.Helper()

	policy, err := newURLPolicy(sourceURL, allowedHostsCSV)
	if err != nil {
		t.Fatalf("newURLPolicy() error = %v", err)
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

func withoutRetrySleep(t *testing.T) {
	t.Helper()

	previous := retrySleepWithJitter
	retrySleepWithJitter = func(context.Context, int) error { return nil }
	t.Cleanup(func() {
		retrySleepWithJitter = previous
	})
}

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

func TestURLPolicyDefaultSourceHostOnly(t *testing.T) {
	policy := mustPolicy(t, "https://code.claude.com/docs/llms.txt", "")

	if err := policy.Validate("https://code.claude.com/docs/en/overview.md"); err != nil {
		t.Fatalf("policy.Validate(source host) error = %v", err)
	}
	if err := policy.Validate("http://code.claude.com/docs/en/overview.md"); err == nil {
		t.Fatal("policy.Validate(http) error = nil, want rejection")
	}
	if err := policy.Validate("https://platform.claude.com/docs/en/overview.md"); err == nil {
		t.Fatal("policy.Validate(cross host) error = nil, want rejection")
	}
	if err := policy.Validate("https://127.0.0.1/docs/en/overview.md"); err == nil {
		t.Fatal("policy.Validate(IP literal) error = nil, want rejection")
	}
}

func TestURLPolicyAllowedHostsOverride(t *testing.T) {
	policy := mustPolicy(t, "https://code.claude.com/docs/llms.txt", "platform.claude.com")

	if err := policy.Validate("https://platform.claude.com/docs/en/overview.md"); err != nil {
		t.Fatalf("policy.Validate(allowed host) error = %v", err)
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
			err := validateResolvedIP(net.ParseIP(tt.ip))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResolvedIP(%s) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
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

	want := filepath.Join("pages", "code.claude.com", "docs", "en", "overview.md")
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLWithQueryInRootLayout(t *testing.T) {
	got, err := relativePathForURL("https://example.com/docs/page.md?lang=en", layoutRoot)
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := filepath.Join("docs", "page__0933497c2075.md")
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestSourcePathForLayout(t *testing.T) {
	if got := sourcePathForLayout(layoutRoot); got != "llms.txt" {
		t.Fatalf("sourcePathForLayout(root) = %q, want %q", got, "llms.txt")
	}
	if got := sourcePathForLayout(layoutNested); got != filepath.ToSlash(filepath.Join("source", "llms.txt")) {
		t.Fatalf("sourcePathForLayout(nested) = %q", got)
	}
}

func TestWriteManifestAndLoadManifestRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	manifestPath := filepath.Join(tempDir, "manifest.json")

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
		Failures: []fetchFailure{
			{URL: "https://example.com/docs/broken.md", Error: "bad response"},
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

func TestEnsureMarkdownResponseRejectsExplicitHTML(t *testing.T) {
	err := ensureMarkdownResponse(
		"200 OK",
		"text/html; charset=utf-8",
		map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		[]byte("<!DOCTYPE html><html><head><title>Not Found</title></head><body></body></html>"),
		"/tmp/body.html",
	)
	if err == nil {
		t.Fatal("ensureMarkdownResponse() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("ensureMarkdownResponse() error = %q, want html rejection", err)
	}
}

func TestEnsureMarkdownResponseRejectsLeadingCommentHTML(t *testing.T) {
	err := ensureMarkdownResponse(
		"200 OK",
		"",
		nil,
		[]byte("\ufeff<!-- banner --><!-- another --><html><body>bad</body></html>"),
		"/tmp/body.html",
	)
	if err == nil {
		t.Fatal("ensureMarkdownResponse() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "HTML document") {
		t.Fatalf("ensureMarkdownResponse() error = %q, want HTML document rejection", err)
	}
}

func TestEnsureMarkdownResponseAcceptsCapturedSkillsMarkdown(t *testing.T) {
	err := ensureMarkdownResponse(
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
		t.Fatalf("ensureMarkdownResponse() error = %v", err)
	}
}

func TestWriteUnexpectedContentDiagnostic(t *testing.T) {
	diagnosticsDir := t.TempDir()
	bodyPath := filepath.Join(t.TempDir(), "body.html")
	body := "<!DOCTYPE html><html><body>bad html</body></html>"
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	unexpected := &unexpectedContentError{
		message:     "expected markdown response but received HTML document",
		status:      "200 OK",
		contentType: "text/html; charset=utf-8",
		headers:     map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		sniff:       []byte(body),
		bodyPath:    bodyPath,
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

func TestFetchDocumentStreamsToDiskAndComputesMetadata(t *testing.T) {
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type":  "text/markdown; charset=utf-8",
			"Last-Modified": "Fri, 13 Mar 2026 06:36:52 GMT",
			"ETag":          `"etag-1"`,
		}, "# Overview\n\nReal markdown content.\n"), nil
	})

	result, err := fetchDocument(
		context.Background(),
		client,
		mustPolicy(t, "https://docs.example.com/llms.txt", ""),
		t.TempDir(),
		t.TempDir(),
		"https://docs.example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		fetchValidators{},
	)
	if err != nil {
		t.Fatalf("fetchDocument() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Overview\n\nReal markdown content.\n" {
		t.Fatalf("fetchDocument() wrote %q", got)
	}
	if result.SHA256 != hashBytes([]byte("# Overview\n\nReal markdown content.\n")) {
		t.Fatalf("fetchDocument() sha256 = %q", result.SHA256)
	}
	if result.Bytes != int64(len("# Overview\n\nReal markdown content.\n")) {
		t.Fatalf("fetchDocument() bytes = %d", result.Bytes)
	}
	if result.LastModifiedAt != "2026-03-13T06:36:52Z" {
		t.Fatalf("fetchDocument() last_modified_at = %q", result.LastModifiedAt)
	}
	if result.ETag != `"etag-1"` {
		t.Fatalf("fetchDocument() etag = %q", result.ETag)
	}
}

func TestFetchDocumentRetriesTransientHTMLForMarkdownURL(t *testing.T) {
	withoutRetrySleep(t)

	var attempts atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/html; charset=utf-8",
			}, "<!DOCTYPE html><html><body>temporary html</body></html>"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Real markdown\n"), nil
	})

	result, err := fetchDocument(
		context.Background(),
		client,
		mustPolicy(t, "https://docs.example.com/llms.txt", ""),
		t.TempDir(),
		t.TempDir(),
		"https://docs.example.com/docs/en/skills.md",
		filepath.Join("docs", "en", "skills.md"),
		fetchValidators{},
	)
	if err != nil {
		t.Fatalf("fetchDocument() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Real markdown\n" {
		t.Fatalf("fetchDocument() body = %q, want markdown body", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("fetchDocument() attempts = %d, want 2", got)
	}
}

func TestFetchDocumentUsesCachedFileOn304(t *testing.T) {
	snapshotRoot := t.TempDir()
	relativePath := filepath.Join("docs", "en", "overview.md")
	fullPath := filepath.Join(snapshotRoot, relativePath)
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

	result, err := fetchDocument(
		context.Background(),
		client,
		mustPolicy(t, "https://docs.example.com/llms.txt", ""),
		t.TempDir(),
		snapshotRoot,
		"https://docs.example.com/docs/en/overview.md",
		relativePath,
		fetchValidators{
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
	)
	if err != nil {
		t.Fatalf("fetchDocument() error = %v", err)
	}

	if result.LocalPath != fullPath {
		t.Fatalf("fetchDocument() local path = %q, want %q", result.LocalPath, fullPath)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if result.ETag != `"etag-2"` {
		t.Fatalf("fetchDocument() etag = %q, want response validator", result.ETag)
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

	result, err := fetchDocument(
		context.Background(),
		client,
		mustPolicy(t, "https://docs.example.com/llms.txt", ""),
		t.TempDir(),
		t.TempDir(),
		"https://docs.example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		fetchValidators{
			LastModifiedAt: "2026-03-13T06:36:52Z",
			ETag:           `"etag-1"`,
		},
	)
	if err != nil {
		t.Fatalf("fetchDocument() error = %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := readFile(t, result.LocalPath); got != "# Refetched markdown\n" {
		t.Fatalf("fetchDocument() refetched body = %q", got)
	}
}

func TestPreservePreviousDocumentUsesExistingSnapshotCopy(t *testing.T) {
	tempDir := t.TempDir()
	body := []byte("# Cached markdown\n")
	targetPath := filepath.Join(tempDir, "docs", "en", "overview.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, body, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	got, err := preservePreviousDocument(
		tempDir,
		"https://example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifestEntry{
			URL:            "https://example.com/docs/en/overview.md",
			Path:           "docs/en/overview.md",
			SHA256:         "stale",
			Bytes:          10,
			LastModifiedAt: "2026-03-13T12:00:00Z",
			ETag:           "\"etag-1\"",
		},
	)
	if err != nil {
		t.Fatalf("preservePreviousDocument() error = %v", err)
	}

	if got.RelativePath != filepath.Join("docs", "en", "overview.md") {
		t.Fatalf("preservePreviousDocument() path = %q", got.RelativePath)
	}
	if got.LocalPath != targetPath {
		t.Fatalf("preservePreviousDocument() local path = %q, want %q", got.LocalPath, targetPath)
	}
	if got.LastModifiedAt != "2026-03-13T12:00:00Z" {
		t.Fatalf("preservePreviousDocument() last_modified_at = %q", got.LastModifiedAt)
	}
	if got.ETag != "\"etag-1\"" {
		t.Fatalf("preservePreviousDocument() etag = %q", got.ETag)
	}
	if got.SHA256 != hashBytes(body) {
		t.Fatalf("preservePreviousDocument() sha256 = %q, want %q", got.SHA256, hashBytes(body))
	}
	if got.Bytes != int64(len(body)) {
		t.Fatalf("preservePreviousDocument() bytes = %d, want %d", got.Bytes, len(body))
	}
}

func TestPreservePreviousDocumentRequiresPreviousSnapshotEntry(t *testing.T) {
	_, err := preservePreviousDocument(
		t.TempDir(),
		"https://example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifestEntry{},
	)
	if err == nil {
		t.Fatal("preservePreviousDocument() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "no previous snapshot entry") {
		t.Fatalf("preservePreviousDocument() error = %v, want missing previous snapshot entry", err)
	}
}

func TestSafeJoinRejectsEscapingPath(t *testing.T) {
	_, err := safeJoin(t.TempDir(), filepath.Join("..", "secret.txt"))
	if err == nil {
		t.Fatal("safeJoin() error = nil, want rejection")
	}
}

func TestFetchDocumentsPreservesPreviousCopyOnFailure(t *testing.T) {
	withoutRetrySleep(t)

	snapshotRoot := t.TempDir()
	cachedBody := []byte("# Previous snapshot\n")
	targetPath := filepath.Join(snapshotRoot, "docs", "en", "skills.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, cachedBody, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/html; charset=utf-8",
		}, "<!DOCTYPE html><html><body>not found</body></html>"), nil
	})

	previous := map[string]manifestEntry{
		"https://docs.example.com/docs/en/skills.md": {
			URL:            "https://docs.example.com/docs/en/skills.md",
			Path:           "docs/en/skills.md",
			SHA256:         hashBytes(cachedBody),
			Bytes:          int64(len(cachedBody)),
			LastModifiedAt: "2026-03-13T12:00:00Z",
			ETag:           "\"etag-1\"",
		},
	}

	gotDocs, gotFailures := fetchDocuments(
		context.Background(),
		client,
		mustPolicy(t, "https://docs.example.com/llms.txt", ""),
		layoutRoot,
		filepath.Join(t.TempDir(), "diagnostics"),
		t.TempDir(),
		snapshotRoot,
		[]string{"https://docs.example.com/docs/en/skills.md"},
		1,
		previous,
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

	if err := replaceDir(tempDir, outputDir); err != nil {
		t.Fatalf("replaceDir() error = %v", err)
	}

	if got := readFile(t, filepath.Join(outputDir, "new.md")); got != "new\n" {
		t.Fatalf("replaceDir() output = %q, want new content", got)
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Fatalf("backup directory still exists: %v", err)
	}
}
