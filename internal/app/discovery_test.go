package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/f-pisani/llmstxt/internal/app"
	"github.com/f-pisani/llmstxt/internal/links"
	"github.com/f-pisani/llmstxt/internal/policy"
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

func TestDiscoverDocumentsHappyPath(t *testing.T) {
	t.Parallel()

	// Root llms.txt links to two docs and a nested llms.txt.
	// The nested llms.txt links to one more doc and a deeper nested llms.txt.
	rootBody := strings.Join([]string{
		"- [Overview](https://docs.example.com/docs/overview.md)",
		"- [Quickstart](https://docs.example.com/docs/quickstart.md)",
		"- [API Index](https://docs.example.com/api/llms.txt)",
	}, "\n") + "\n"
	nestedBody := "- [Endpoints](https://docs.example.com/docs/endpoints.md)\n- [Deep](https://docs.example.com/api/v2/llms.txt)\n"
	deepBody := "- [Migration](https://docs.example.com/docs/migration.md)\n"

	client := newTestClient(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, nestedBody), nil
		case "/api/v2/llms.txt":
			return testResponse(200, map[string]string{"Content-Type": "text/plain"}, deepBody), nil
		default:
			return testResponse(404, nil, "not found"), nil
		}
	})

	pol := mustPolicy(t, "https://docs.example.com/llms.txt", "")
	extractedLinks, err := links.Extract([]byte(rootBody))
	if err != nil {
		t.Fatalf("links.Extract() error = %v", err)
	}

	result, err := app.DiscoverDocuments(
		context.Background(), "https://docs.example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client:      client,
			URLPolicy:   pol,
			SpoolDir:    t.TempDir(),
			ArchiveRoot: t.TempDir(),
			Layout:      links.LayoutRoot,
		},
	)
	if err != nil {
		t.Fatalf("DiscoverDocuments() error = %v", err)
	}

	wantDocs := map[string]bool{
		"https://docs.example.com/docs/overview.md":   true,
		"https://docs.example.com/docs/quickstart.md": true,
		"https://docs.example.com/docs/endpoints.md":  true,
		"https://docs.example.com/docs/migration.md":  true,
	}
	if len(result.DocURLs) != len(wantDocs) {
		t.Fatalf("got %d docs, want %d: %v", len(result.DocURLs), len(wantDocs), result.DocURLs)
	}
	for _, u := range result.DocURLs {
		if !wantDocs[u] {
			t.Errorf("unexpected doc URL: %s", u)
		}
	}
	if len(result.IndexResults) != 2 {
		t.Fatalf("got %d index results, want 2", len(result.IndexResults))
	}
}

func TestDiscoverDocumentsCyclePrevention(t *testing.T) {
	t.Parallel()

	bodyA := "- [B](https://docs.example.com/b/llms.txt)\n- [Doc](https://docs.example.com/a.md)\n"
	bodyB := "- [A](https://docs.example.com/a/llms.txt)\n- [Doc](https://docs.example.com/b.md)\n"

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

	pol := mustPolicy(t, "https://docs.example.com/llms.txt", "")

	// Start from root which links to /a/llms.txt.
	rootBody := "- [A](https://docs.example.com/a/llms.txt)\n"
	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://docs.example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client:      client,
			URLPolicy:   pol,
			SpoolDir:    t.TempDir(),
			ArchiveRoot: t.TempDir(),
			Layout:      links.LayoutRoot,
		},
	)
	if err != nil {
		t.Fatalf("DiscoverDocuments() error = %v", err)
	}

	// Should discover docs from both indexes without infinite loop.
	if len(result.DocURLs) != 2 {
		t.Fatalf("got %d docs, want 2: %v", len(result.DocURLs), result.DocURLs)
	}
	if len(result.IndexResults) != 2 {
		t.Fatalf("got %d indexes, want 2", len(result.IndexResults))
	}
}

func TestDiscoverDocumentsCrossHostBlocked(t *testing.T) {
	t.Parallel()

	rootBody := "- [Cross](https://other.com/llms.txt)\n- [Doc](https://docs.example.com/a.md)\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(404, nil, "not found"), nil
	})

	pol := mustPolicy(t, "https://docs.example.com/llms.txt", "")
	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://docs.example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client:      client,
			URLPolicy:   pol,
			SpoolDir:    t.TempDir(),
			ArchiveRoot: t.TempDir(),
			Layout:      links.LayoutRoot,
		},
	)
	if err != nil {
		t.Fatalf("DiscoverDocuments() error = %v", err)
	}

	if len(result.DocURLs) != 1 {
		t.Fatalf("got %d docs, want 1", len(result.DocURLs))
	}

	foundSkipped := false
	for _, s := range result.Skipped {
		if s.URL == "https://other.com/llms.txt" && strings.Contains(s.Reason, "policy") {
			foundSkipped = true
			break
		}
	}
	if !foundSkipped {
		t.Fatalf("cross-host index not in skipped list: %v", result.Skipped)
	}
}

func TestDiscoverDocumentsCapReached(t *testing.T) {
	t.Parallel()

	// Generate more than 50 unique nested indexes.
	var rootLinks []string
	for i := 0; i < 60; i++ {
		rootLinks = append(rootLinks, fmt.Sprintf("- [Idx%d](https://docs.example.com/%d/llms.txt)", i, i))
	}
	rootBody := strings.Join(rootLinks, "\n") + "\n"

	client := newTestClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(200, map[string]string{"Content-Type": "text/plain"}, "- [Doc](https://docs.example.com/doc.md)\n"), nil
	})

	pol := mustPolicy(t, "https://docs.example.com/llms.txt", "")
	extractedLinks, _ := links.Extract([]byte(rootBody))

	result, err := app.DiscoverDocuments(
		context.Background(), "https://docs.example.com/llms.txt", extractedLinks, app.DiscoveryConfig{
			Client:      client,
			URLPolicy:   pol,
			SpoolDir:    t.TempDir(),
			ArchiveRoot: t.TempDir(),
			Layout:      links.LayoutRoot,
		},
	)
	if err != nil {
		t.Fatalf("DiscoverDocuments() error = %v", err)
	}

	if len(result.IndexResults) > 50 {
		t.Fatalf("got %d index results, want <= 50 (cap should be enforced)", len(result.IndexResults))
	}
}
