package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	app "github.com/f-pisani/llmstxt/internal/app"
	"github.com/f-pisani/llmstxt/internal/fetch"
	"github.com/f-pisani/llmstxt/internal/links"
	"github.com/f-pisani/llmstxt/internal/manifest"
	"github.com/f-pisani/llmstxt/internal/policy"

	"golang.org/x/time/rate"
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
	pol, err := policy.NewURLPolicy(sourceURL, allowedHostsCSV)
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	return pol
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- test helper
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	return string(body)
}

// noopRetrySleep skips all retry delays in tests.
func noopRetrySleep(_ context.Context, _ int) error { return nil }

func TestDocumentHappyPath(t *testing.T) {
	t.Parallel()
	markdownBody := "# Overview\n\nSome documentation content.\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type":  "text/markdown; charset=utf-8",
			"Last-Modified": "Sat, 01 Mar 2025 12:00:00 GMT",
			"ETag":          `"doc-etag-1"`,
		}, markdownBody), nil
	})

	spoolDir := t.TempDir()
	archiveRoot := t.TempDir()

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		filepath.Join("docs", "en", "overview.md"),
		manifest.Entry{},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    spoolDir,
				ArchiveRoot: archiveRoot,
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != markdownBody {
		t.Fatalf("body = %q, want %q", got, markdownBody)
	}
	if result.SHA256 != fetch.HashBytes([]byte(markdownBody)) {
		t.Fatalf("SHA256 = %q, want %q", result.SHA256, fetch.HashBytes([]byte(markdownBody)))
	}
	if result.Bytes != int64(len(markdownBody)) {
		t.Fatalf("Bytes = %d, want %d", result.Bytes, len(markdownBody))
	}
	if result.LastModifiedAt != "2025-03-01T12:00:00Z" {
		t.Fatalf("LastModifiedAt = %q", result.LastModifiedAt)
	}
	if result.ETag != `"doc-etag-1"` {
		t.Fatalf("ETag = %q", result.ETag)
	}
	if result.URL != "https://docs.example.com/docs/en/overview.md" {
		t.Fatalf("URL = %q", result.URL)
	}
	if result.RelativePath != filepath.Join("docs", "en", "overview.md") {
		t.Fatalf("RelativePath = %q", result.RelativePath)
	}
}

func TestDocumentRetriesTransientHTML(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/html; charset=utf-8",
			}, "<!DOCTYPE html><html><body>error page</body></html>"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Real content\n"), nil
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
			RetrySleep: noopRetrySleep,
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Real content\n" {
		t.Fatalf("body = %q, want markdown", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDocument304NotModified(t *testing.T) {
	t.Parallel()

	archiveRoot := t.TempDir()
	relativePath := filepath.Join("docs", "en", "overview.md")
	fullPath := filepath.Join(archiveRoot, relativePath)
	cachedBody := "# Cached markdown\n"

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(cachedBody), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	var requests atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if got := req.Header.Get("If-None-Match"); got != `"etag-cached"` {
			t.Errorf("If-None-Match = %q, want %q", got, `"etag-cached"`)
		}
		return testResponse(http.StatusNotModified, map[string]string{
			"ETag": `"etag-updated"`,
		}, ""), nil
	})

	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/docs/en/overview.md",
		relativePath,
		manifest.Entry{
			Path:           filepath.ToSlash(relativePath),
			SHA256:         fetch.HashBytes([]byte(cachedBody)),
			Bytes:          int64(len(cachedBody)),
			LastModifiedAt: "2025-03-01T12:00:00Z",
			ETag:           `"etag-cached"`,
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
		t.Fatalf("LocalPath = %q, want cached path %q", result.LocalPath, fullPath)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if result.ETag != `"etag-updated"` {
		t.Fatalf("ETag = %q, want updated etag", result.ETag)
	}
}

func TestDocument304CacheMiss(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return testResponse(http.StatusNotModified, map[string]string{
				"ETag": `"etag-1"`,
			}, ""), nil
		default:
			if got := req.Header.Get("If-None-Match"); got != "" {
				t.Errorf("If-None-Match on refetch = %q, want empty", got)
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
			LastModifiedAt: "2025-01-01T00:00:00Z",
			ETag:           `"etag-1"`,
		},
		fetch.DocumentConfig{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(), // empty archive, cache miss
			},
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (304 + refetch)", got)
	}
	if got := readFile(t, result.LocalPath); got != "# Refetched markdown\n" {
		t.Fatalf("body = %q, want refetched content", got)
	}
}

func TestDocumentTransientHTTPError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return testResponse(http.StatusInternalServerError, nil, "internal error"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Recovered\n"), nil
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
			RetrySleep: noopRetrySleep,
		},
	)
	if err != nil {
		t.Fatalf("fetch.Document() error = %v", err)
	}

	if got := readFile(t, result.LocalPath); got != "# Recovered\n" {
		t.Fatalf("body = %q, want recovered content", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDocumentNonMarkdownURL(t *testing.T) {
	t.Parallel()

	txtBody := "plain text content\n"
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/plain; charset=utf-8",
		}, txtBody), nil
	})

	// llms.txt is not a .md URL, so no markdown validation is applied.
	result, err := fetch.Document(
		context.Background(),
		"https://docs.example.com/llms.txt",
		"llms.txt",
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

	if got := readFile(t, result.LocalPath); got != txtBody {
		t.Fatalf("body = %q, want %q", got, txtBody)
	}
	if result.SHA256 != fetch.HashBytes([]byte(txtBody)) {
		t.Fatalf("SHA256 mismatch")
	}
}

func TestDocumentsParallelFetch(t *testing.T) {
	t.Parallel()

	pages := map[string]string{
		"https://docs.example.com/docs/a.md": "# Page A\n",
		"https://docs.example.com/docs/b.md": "# Page B\n",
		"https://docs.example.com/docs/c.md": "# Page C\n",
		"https://docs.example.com/docs/d.md": "# Page D\n",
	}

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		fullURL := req.URL.String()
		body, ok := pages[fullURL]
		if !ok {
			return testResponse(http.StatusNotFound, nil, "not found"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, body), nil
	})

	urls := make([]string, 0, len(pages))
	for u := range pages {
		urls = append(urls, u)
	}

	docs, failures := fetch.Documents(
		context.Background(),
		urls,
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
			Layout:      links.LayoutRoot,
			Concurrency: 4,
			RetrySleep:  noopRetrySleep,
		},
	)

	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(docs) != 4 {
		t.Fatalf("docs len = %d, want 4", len(docs))
	}

	gotURLs := make(map[string]bool)
	for _, d := range docs {
		gotURLs[d.URL] = true
		body := readFile(t, d.LocalPath)
		if body != pages[d.URL] {
			t.Errorf("body for %s = %q, want %q", d.URL, body, pages[d.URL])
		}
	}
	for u := range pages {
		if !gotURLs[u] {
			t.Errorf("missing result for %s", u)
		}
	}
}

func TestDocumentsPreservesPreviousOnFailure(t *testing.T) {
	t.Parallel()

	archiveRoot := t.TempDir()
	cachedBody := []byte("# Previous snapshot\n")
	targetPath := filepath.Join(archiveRoot, "docs", "en", "failing.md")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(targetPath, cachedBody, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	// Always return HTML for the failing URL.
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/html; charset=utf-8",
		}, "<!DOCTYPE html><html><body>error</body></html>"), nil
	})

	previous := map[string]manifest.Entry{
		"https://docs.example.com/docs/en/failing.md": {
			URL:            "https://docs.example.com/docs/en/failing.md",
			Path:           "docs/en/failing.md",
			SHA256:         fetch.HashBytes(cachedBody),
			Bytes:          int64(len(cachedBody)),
			LastModifiedAt: "2025-03-01T12:00:00Z",
			ETag:           `"etag-prev"`,
		},
	}

	docs, failures := fetch.Documents(
		context.Background(),
		[]string{"https://docs.example.com/docs/en/failing.md"},
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
			RetrySleep:     noopRetrySleep,
		},
	)

	if len(docs) != 1 {
		t.Fatalf("docs len = %d, want 1 (preserved copy)", len(docs))
	}
	if docs[0].LocalPath != targetPath {
		t.Fatalf("preserved local path = %q, want %q", docs[0].LocalPath, targetPath)
	}
	if len(failures) != 1 {
		t.Fatalf("failures len = %d, want 1", len(failures))
	}
	if !failures[0].PreservedExisting {
		t.Fatal("failure.PreservedExisting = false, want true")
	}
}

func TestDocumentsRateLimiter(t *testing.T) {
	t.Parallel()

	var fetchCount atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		fetchCount.Add(1)
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Doc\n"), nil
	})

	urls := []string{
		"https://docs.example.com/docs/a.md",
		"https://docs.example.com/docs/b.md",
		"https://docs.example.com/docs/c.md",
	}

	// Rate limiter: 100 req/s so tests run fast but we verify it's wired.
	limiter := rate.NewLimiter(rate.Limit(100), 101)

	start := time.Now()
	docs, failures := fetch.Documents(
		context.Background(),
		urls,
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
			Layout:      links.LayoutRoot,
			Concurrency: 2,
			RateLimiter: limiter,
			RetrySleep:  noopRetrySleep,
		},
	)
	elapsed := time.Since(start)

	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(docs) != 3 {
		t.Fatalf("docs len = %d, want 3", len(docs))
	}
	if got := fetchCount.Load(); got != 3 {
		t.Fatalf("fetchCount = %d, want 3", got)
	}
	// With 100 req/s, 3 requests should complete well under 5 seconds.
	if elapsed > 5*time.Second {
		t.Fatalf("rate-limited fetch took %v, expected < 5s", elapsed)
	}
}

func TestDocumentsPreviousPathTakesPrecedence(t *testing.T) {
	t.Parallel()

	markdownBody := "# Stable content\n"
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, markdownBody), nil
	})

	docs, failures := fetch.Documents(
		context.Background(),
		[]string{"https://docs.example.com/new/location.md"},
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
			Layout:      links.LayoutRoot,
			Concurrency: 1,
			PreviousDocs: map[string]manifest.Entry{
				"https://docs.example.com/new/location.md": {
					URL:    "https://docs.example.com/new/location.md",
					Path:   "old/location.md",
					SHA256: fetch.HashBytes([]byte(markdownBody)),
					Bytes:  int64(len(markdownBody)),
				},
			},
			RetrySleep: noopRetrySleep,
		},
	)

	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(docs) != 1 {
		t.Fatalf("docs len = %d, want 1", len(docs))
	}
	if docs[0].RelativePath != "old/location.md" {
		t.Fatalf("RelativePath = %q, want %q", docs[0].RelativePath, "old/location.md")
	}
}

func TestDocumentsOutputOrderMatchesInputOrder(t *testing.T) {
	t.Parallel()

	urls := []string{
		"https://docs.example.com/docs/a.md",
		"https://docs.example.com/docs/b.md",
		"https://docs.example.com/docs/c.md",
		"https://docs.example.com/docs/d.md",
		"https://docs.example.com/docs/e.md",
	}

	var counter atomic.Int32
	gate := make(chan struct{})

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		n := counter.Add(1)
		if n == 1 {
			// Block the first URL until another request arrives.
			<-gate
		} else if n == 2 {
			// Unblock the first URL once the second request starts.
			close(gate)
		}
		body := fmt.Sprintf("# %s\n", req.URL.Path)
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, body), nil
	})

	docs, failures := fetch.Documents(
		context.Background(),
		urls,
		fetch.Options{
			ClientConfig: fetch.ClientConfig{
				Client:      client,
				URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
				SpoolDir:    t.TempDir(),
				ArchiveRoot: t.TempDir(),
			},
			Layout:      links.LayoutRoot,
			Concurrency: 5,
			RetrySleep:  noopRetrySleep,
		},
	)

	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(docs) != len(urls) {
		t.Fatalf("docs len = %d, want %d", len(docs), len(urls))
	}
	for i, u := range urls {
		if docs[i].URL != u {
			t.Fatalf("docs[%d].URL = %q, want %q", i, docs[i].URL, u)
		}
	}
}

func TestDocumentMarkdownExhaustsRetriesReturnsUnexpectedContentError(t *testing.T) {
	t.Parallel()

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/html; charset=utf-8",
		}, "<!DOCTYPE html><html><body>error page</body></html>"), nil
	})

	_, err := fetch.Document(
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
			RetrySleep: noopRetrySleep,
		},
	)
	if err == nil {
		t.Fatal("fetch.Document() error = nil, want UnexpectedContentError")
	}

	var unexpected *fetch.UnexpectedContentError
	if !errors.As(err, &unexpected) {
		t.Fatalf("error type = %T, want *fetch.UnexpectedContentError", err)
	}
}

func TestDocumentsContextCancellation(t *testing.T) {
	t.Parallel()

	urls := make([]string, 10)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://docs.example.com/docs/%d.md", i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var served atomic.Int32
	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		if served.Add(1) >= 3 {
			cancel()
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, "# Doc\n"), nil
	})

	done := make(chan struct{})
	var docs []fetch.Result
	go func() {
		defer close(done)
		docs, _ = fetch.Documents(
			ctx,
			urls,
			fetch.Options{
				ClientConfig: fetch.ClientConfig{
					Client:      client,
					URLPolicy:   mustPolicy(t, "https://docs.example.com/llms.txt", ""),
					SpoolDir:    t.TempDir(),
					ArchiveRoot: t.TempDir(),
				},
				Layout:      links.LayoutRoot,
				Concurrency: 1,
				RetrySleep:  noopRetrySleep,
			},
		)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Documents() did not return within 10 seconds after context cancellation")
	}

	if len(docs) >= 10 {
		t.Fatalf("docs len = %d, want < 10 (context was cancelled)", len(docs))
	}
	for i, d := range docs {
		if d.URL == "" {
			t.Fatalf("docs[%d].URL is empty, want non-empty for all returned results", i)
		}
	}
}

func TestDocumentsPreservedFailureInBuildManifest(t *testing.T) {
	t.Parallel()

	archiveRoot := t.TempDir()
	cachedBody := []byte("# Cached doc\n")
	cachedPath := filepath.Join(archiveRoot, "docs", "failing.md")
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(cachedPath, cachedBody, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	successBody := "# Success doc\n"
	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "failing") {
			return testResponse(http.StatusOK, map[string]string{
				"Content-Type": "text/html; charset=utf-8",
			}, "<!DOCTYPE html><html><body>error</body></html>"), nil
		}
		return testResponse(http.StatusOK, map[string]string{
			"Content-Type": "text/markdown; charset=utf-8",
		}, successBody), nil
	})

	previous := map[string]manifest.Entry{
		"https://docs.example.com/docs/failing.md": {
			URL:    "https://docs.example.com/docs/failing.md",
			Path:   "docs/failing.md",
			SHA256: fetch.HashBytes(cachedBody),
			Bytes:  int64(len(cachedBody)),
		},
	}

	docs, failures := fetch.Documents(
		context.Background(),
		[]string{
			"https://docs.example.com/docs/success.md",
			"https://docs.example.com/docs/failing.md",
		},
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
			RetrySleep:     noopRetrySleep,
		},
	)

	if len(docs) != 2 {
		t.Fatalf("docs len = %d, want 2", len(docs))
	}

	source := fetch.Result{
		URL:          "https://docs.example.com/llms.txt",
		RelativePath: "llms.txt",
		SHA256:       fetch.HashBytes([]byte("source")),
	}

	m := app.BuildManifest(source, docs, nil, failures, nil)

	if m.DocumentCount != 2 {
		t.Fatalf("m.DocumentCount = %d, want 2", m.DocumentCount)
	}
	if len(m.Failures) != 1 {
		t.Fatalf("len(m.Failures) = %d, want 1", len(m.Failures))
	}
	if !m.Failures[0].PreservedExisting {
		t.Fatal("m.Failures[0].PreservedExisting = false, want true")
	}
}
