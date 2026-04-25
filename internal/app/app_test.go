package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llms-txt-archive/llmstxt/internal/fetch"
	"github.com/llms-txt-archive/llmstxt/internal/manifest"
)

func TestResultToEntry(t *testing.T) {
	tests := []struct {
		name   string
		result fetch.Result
		want   manifest.Entry
	}{
		{
			name: "basic conversion",
			result: fetch.Result{
				URL:            "https://example.com/docs/intro.md",
				RelativePath:   "docs/intro.md",
				LocalPath:      "/tmp/spool/intro.md",
				SHA256:         "abc123",
				Bytes:          1024,
				LastModifiedAt: "2025-01-01T00:00:00Z",
				ETag:           `"etag-1"`,
			},
			want: manifest.Entry{
				URL:            "https://example.com/docs/intro.md",
				Path:           "docs/intro.md",
				SHA256:         "abc123",
				Bytes:          1024,
				LastModifiedAt: "2025-01-01T00:00:00Z",
				ETag:           `"etag-1"`,
			},
		},
		{
			name: "backslash path normalized to forward slash",
			result: fetch.Result{
				URL:          "https://example.com/a.md",
				RelativePath: filepath.Join("docs", "sub", "a.md"),
				SHA256:       "def456",
				Bytes:        512,
			},
			want: manifest.Entry{
				URL:    "https://example.com/a.md",
				Path:   "docs/sub/a.md",
				SHA256: "def456",
				Bytes:  512,
			},
		},
		{
			name:   "empty result",
			result: fetch.Result{},
			want:   manifest.Entry{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resultToEntry(tt.result)
			if got != tt.want {
				t.Errorf("resultToEntry() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBuildManifest(t *testing.T) {
	source := fetch.Result{
		URL:            "https://example.com/llms.txt",
		RelativePath:   "llms.txt",
		SHA256:         "src-sha",
		LastModifiedAt: "2025-06-01T00:00:00Z",
		ETag:           `"src-etag"`,
	}

	docs := []fetch.Result{
		{URL: "https://example.com/a.md", RelativePath: "a.md", SHA256: "aaa", Bytes: 100},
		{URL: "https://example.com/b.md", RelativePath: "b.md", SHA256: "bbb", Bytes: 200},
	}

	skipped := []manifest.SkippedEntry{
		{URL: "https://example.com/skip.md", Reason: "policy: blocked"},
	}

	failures := []manifest.FetchFailure{
		{URL: "https://example.com/fail.md", Error: "timeout"},
	}

	indexes := []fetch.Result{
		{URL: "https://example.com/sub/llms.txt", RelativePath: "sources/sub-llms.txt", SHA256: "idx-sha", ETag: `"idx-etag"`},
	}

	m := BuildManifest(source, docs, skipped, failures, indexes, nil)

	if m.SourceURL != source.URL {
		t.Errorf("SourceURL = %q, want %q", m.SourceURL, source.URL)
	}
	if m.SourcePath != "llms.txt" {
		t.Errorf("SourcePath = %q, want %q", m.SourcePath, "llms.txt")
	}
	if m.SourceSHA256 != "src-sha" {
		t.Errorf("SourceSHA256 = %q, want %q", m.SourceSHA256, "src-sha")
	}
	if m.SourceLastModifiedAt != "2025-06-01T00:00:00Z" {
		t.Errorf("SourceLastModifiedAt = %q", m.SourceLastModifiedAt)
	}
	if m.SourceETag != `"src-etag"` {
		t.Errorf("SourceETag = %q", m.SourceETag)
	}
	if m.DocumentCount != 2 {
		t.Errorf("DocumentCount = %d, want 2", m.DocumentCount)
	}
	if m.SkippedCount != 1 {
		t.Errorf("SkippedCount = %d, want 1", m.SkippedCount)
	}
	if len(m.Documents) != 2 {
		t.Fatalf("len(Documents) = %d, want 2", len(m.Documents))
	}
	if m.Documents[0].URL != "https://example.com/a.md" {
		t.Errorf("Documents[0].URL = %q", m.Documents[0].URL)
	}
	if len(m.Skipped) != 1 {
		t.Fatalf("len(Skipped) = %d, want 1", len(m.Skipped))
	}
	if len(m.Failures) != 1 {
		t.Fatalf("len(Failures) = %d, want 1", len(m.Failures))
	}
	if len(m.Sources) != 1 {
		t.Fatalf("len(Sources) = %d, want 1", len(m.Sources))
	}
	if m.Sources[0].URL != "https://example.com/sub/llms.txt" {
		t.Errorf("Sources[0].URL = %q", m.Sources[0].URL)
	}
	if m.Sources[0].Path != "sources/sub-llms.txt" {
		t.Errorf("Sources[0].Path = %q", m.Sources[0].Path)
	}

	t.Run("no discovered indexes", func(t *testing.T) {
		m2 := BuildManifest(source, docs, nil, nil, nil, nil)
		if m2.Sources != nil {
			t.Errorf("Sources should be nil when no discovered indexes, got %v", m2.Sources)
		}
	})

	t.Run("no skipped or failures", func(t *testing.T) {
		m3 := BuildManifest(source, docs, nil, nil, nil, nil)
		if m3.Skipped != nil {
			t.Errorf("Skipped should be nil, got %v", m3.Skipped)
		}
		if m3.Failures != nil {
			t.Errorf("Failures should be nil, got %v", m3.Failures)
		}
	})
}

func TestBuildDiagnosticManifest(t *testing.T) {
	t.Run("nil source does not panic", func(t *testing.T) {
		failures := []manifest.FetchFailure{
			{URL: "https://example.com/llms.txt", Error: "connection refused"},
		}
		m := BuildDiagnosticManifest("https://example.com/llms.txt", "llms.txt", nil, nil, nil, failures)

		if m.SourceURL != "https://example.com/llms.txt" {
			t.Errorf("SourceURL = %q", m.SourceURL)
		}
		if m.SourceSHA256 != "" {
			t.Errorf("SourceSHA256 should be empty when source is nil, got %q", m.SourceSHA256)
		}
		if len(m.Failures) != 1 {
			t.Fatalf("len(Failures) = %d, want 1", len(m.Failures))
		}
	})

	t.Run("with source result", func(t *testing.T) {
		src := &fetch.Result{
			SHA256:         "diag-sha",
			LastModifiedAt: "2025-03-01T00:00:00Z",
			ETag:           `"diag-etag"`,
		}
		docs := []fetch.Result{
			{URL: "https://example.com/d.md", RelativePath: "d.md", SHA256: "ddd", Bytes: 50},
		}
		m := BuildDiagnosticManifest("https://example.com/llms.txt", "llms.txt", src, docs, nil, nil)

		if m.SourceSHA256 != "diag-sha" {
			t.Errorf("SourceSHA256 = %q, want %q", m.SourceSHA256, "diag-sha")
		}
		if m.DocumentCount != 1 {
			t.Errorf("DocumentCount = %d, want 1", m.DocumentCount)
		}
		if len(m.Documents) != 1 {
			t.Fatalf("len(Documents) = %d, want 1", len(m.Documents))
		}
		if m.Documents[0].SHA256 != "ddd" {
			t.Errorf("Documents[0].SHA256 = %q", m.Documents[0].SHA256)
		}
	})

	t.Run("with skipped entries", func(t *testing.T) {
		skipped := []manifest.SkippedEntry{
			{URL: "https://example.com/skip.md", Reason: "policy: blocked"},
		}
		m := BuildDiagnosticManifest("https://example.com/llms.txt", "llms.txt", nil, nil, skipped, nil)
		if m.SkippedCount != 1 {
			t.Errorf("SkippedCount = %d, want 1", m.SkippedCount)
		}
		if len(m.Skipped) != 1 {
			t.Fatalf("len(Skipped) = %d, want 1", len(m.Skipped))
		}
	})

	t.Run("empty documents are not added", func(t *testing.T) {
		m := BuildDiagnosticManifest("https://example.com/llms.txt", "llms.txt", nil, nil, nil, nil)
		if m.Documents != nil {
			t.Errorf("Documents should be nil when empty, got %v", m.Documents)
		}
	})
}

func TestWriteDiagnosticManifest(t *testing.T) {
	t.Run("empty path is a no-op", func(_ *testing.T) {
		// Should not panic or create files.
		WriteDiagnosticManifest("", manifest.Manifest{})
	})

	t.Run("writes manifest to valid path", func(t *testing.T) {
		dir := t.TempDir()
		outPath := filepath.Join(dir, "diag", "manifest.json")
		m := manifest.Manifest{
			SourceURL:     "https://example.com/llms.txt",
			DocumentCount: 0,
		}
		WriteDiagnosticManifest(outPath, m)

		data, err := os.ReadFile(outPath) // #nosec G304 -- test reads temp file it created
		if err != nil {
			t.Fatalf("expected file to be written: %v", err)
		}

		var loaded manifest.Manifest
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Fatalf("failed to parse written manifest: %v", err)
		}
		if loaded.SourceURL != "https://example.com/llms.txt" {
			t.Errorf("SourceURL = %q", loaded.SourceURL)
		}
	})
}

func TestSkippedEntry(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		reason     string
		err        error
		wantURL    string
		wantReason string
	}{
		{
			name:       "formats reason with error",
			url:        "https://example.com/bad.md",
			reason:     "policy",
			err:        os.ErrPermission,
			wantURL:    "https://example.com/bad.md",
			wantReason: "policy: permission denied",
		},
		{
			name:       "fetch failed reason",
			url:        "https://example.com/timeout.md",
			reason:     "fetch failed",
			err:        os.ErrDeadlineExceeded,
			wantURL:    "https://example.com/timeout.md",
			wantReason: "fetch failed: i/o timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := skippedEntry(tt.url, tt.reason, tt.err)
			if got.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tt.wantURL)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestNormalizeIndexURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "strips fragment",
			raw:  "https://example.com/llms.txt#section",
			want: "https://example.com/llms.txt",
		},
		{
			name: "no fragment unchanged",
			raw:  "https://example.com/llms.txt",
			want: "https://example.com/llms.txt",
		},
		{
			name: "preserves query string",
			raw:  "https://example.com/llms.txt?v=2#frag",
			want: "https://example.com/llms.txt?v=2",
		},
		{
			name: "invalid URL passthrough",
			raw:  "://not-a-url",
			want: "://not-a-url",
		},
		{
			name: "empty string",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeIndexURL(tt.raw)
			if got != tt.want {
				t.Errorf("normalizeIndexURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestPartialSyncErrorError(t *testing.T) {
	t.Run("zero failures", func(t *testing.T) {
		e := &PartialSyncError{}
		if e.Error() != "partial sync" {
			t.Errorf("Error() = %q, want %q", e.Error(), "partial sync")
		}
	})

	t.Run("multiple failures", func(t *testing.T) {
		e := &PartialSyncError{
			Failures: []manifest.FetchFailure{
				{URL: "https://example.com/a.md", Error: "timeout"},
				{URL: "https://example.com/b.md", Error: "404"},
			},
		}
		msg := e.Error()

		if !strings.HasPrefix(msg, "2 fetches failed") {
			t.Errorf("expected message to start with '2 fetches failed', got %q", msg)
		}
		if !strings.Contains(msg, "- https://example.com/a.md: timeout") {
			t.Errorf("expected failure line for a.md, got %q", msg)
		}
		if !strings.Contains(msg, "- https://example.com/b.md: 404") {
			t.Errorf("expected failure line for b.md, got %q", msg)
		}
	})

	t.Run("single failure", func(t *testing.T) {
		e := &PartialSyncError{
			Failures: []manifest.FetchFailure{
				{URL: "https://example.com/x.md", Error: "connection reset"},
			},
		}
		msg := e.Error()
		if !strings.HasPrefix(msg, "1 fetches failed") {
			t.Errorf("expected '1 fetches failed' prefix, got %q", msg)
		}
	})
}

func TestBuildManifestPreservesFailedSources(t *testing.T) {
	source := fetch.Result{
		URL:          "https://example.com/llms.txt",
		RelativePath: "llms.txt",
		SHA256:       "src-sha",
	}

	// A nested index failed — it should still appear in Sources
	// so the next run can preserve docs via PreviousSourceDocURLs.
	failedSources := []manifest.SourceEntry{
		{URL: "https://example.com/api/llms.txt", Path: "api/llms.txt"},
	}

	m := BuildManifest(source, nil, nil, nil, nil, failedSources)

	if len(m.Sources) != 1 {
		t.Fatalf("Sources = %d, want 1 (failed source should be preserved)", len(m.Sources))
	}
	if m.Sources[0].URL != "https://example.com/api/llms.txt" {
		t.Errorf("Sources[0].URL = %q, want failed index URL", m.Sources[0].URL)
	}
}

func TestBuildManifestMergesSuccessfulAndFailedSources(t *testing.T) {
	source := fetch.Result{
		URL:          "https://example.com/llms.txt",
		RelativePath: "llms.txt",
		SHA256:       "src-sha",
	}

	successIndexes := []fetch.Result{
		{URL: "https://example.com/v1/llms.txt", RelativePath: "v1/llms.txt", SHA256: "idx-1"},
	}
	failedSources := []manifest.SourceEntry{
		{URL: "https://example.com/v2/llms.txt", Path: "v2/llms.txt"},
	}

	m := BuildManifest(source, nil, nil, nil, successIndexes, failedSources)

	if len(m.Sources) != 2 {
		t.Fatalf("Sources = %d, want 2 (1 successful + 1 failed)", len(m.Sources))
	}
	urls := map[string]bool{}
	for _, s := range m.Sources {
		urls[s.URL] = true
	}
	if !urls["https://example.com/v1/llms.txt"] {
		t.Error("missing successful index in Sources")
	}
	if !urls["https://example.com/v2/llms.txt"] {
		t.Error("missing failed index in Sources")
	}
}

func TestDefaultLogger(t *testing.T) {
	t.Run("nil returns slog.Default", func(t *testing.T) {
		cfg := Config{}
		got := cfg.logger()
		if got != slog.Default() {
			t.Errorf("expected slog.Default(), got different logger")
		}
	})

	t.Run("non-nil returns provided logger", func(t *testing.T) {
		custom := slog.New(slog.NewTextHandler(os.Stderr, nil))
		cfg := Config{Logger: custom}
		got := cfg.logger()
		if got != custom {
			t.Errorf("expected custom logger, got different logger")
		}
	})
}
