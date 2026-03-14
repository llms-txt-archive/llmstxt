package links

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestExtractMarkdownLinks(t *testing.T) {
	body := []byte("- [Doc A](https://example.com/a.md)\n- [Doc B](https://example.com/b.md)\n")
	urls, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("Extract() = %d URLs, want 2", len(urls))
	}
}

func TestExtractPlainURLs(t *testing.T) {
	body := []byte("https://example.com/a.md\nhttps://example.com/b.md\n")
	urls, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("Extract() = %d URLs, want 2", len(urls))
	}
}

func TestExtractDedup(t *testing.T) {
	body := []byte("- [A](https://example.com/a.md)\n- [A again](https://example.com/a.md)\n")
	urls, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(urls) != 1 {
		t.Fatalf("Extract() = %d URLs, want 1 (dedup)", len(urls))
	}
}

func TestExtractSorted(t *testing.T) {
	body := []byte("- [B](https://example.com/b.md)\n- [A](https://example.com/a.md)\n")
	urls, err := Extract(body)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if urls[0] != "https://example.com/a.md" {
		t.Fatalf("Extract() not sorted: got %v", urls)
	}
}

func TestExtractNoURLs(t *testing.T) {
	_, err := Extract([]byte("no links here"))
	if !errors.Is(err, ErrNoDocumentURLs) {
		t.Fatalf("Extract() error = %v, want ErrNoDocumentURLs", err)
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
		{"https://example.com/doc.md", false},
	}
	for _, tt := range tests {
		if got := IsIndex(tt.url); got != tt.want {
			t.Errorf("IsIndex(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestIsMarkdown(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://example.com/doc.md", true},
		{"https://example.com/doc.MD", true},
		{"https://example.com/doc.txt", false},
		{"https://example.com/doc", false},
	}
	for _, tt := range tests {
		got, err := IsMarkdown(tt.url)
		if err != nil {
			t.Errorf("IsMarkdown(%q) error = %v", tt.url, err)
			continue
		}
		if got != tt.want {
			t.Errorf("IsMarkdown(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestPartitionMixed(t *testing.T) {
	links := []string{
		"https://example.com/doc.md",
		"https://example.com/llms.txt",
		"https://example.com/page.html",
	}
	docs, indexes, skipped, err := Partition(links)
	if err != nil {
		t.Fatalf("Partition() error = %v", err)
	}
	if len(docs) != 1 || docs[0] != "https://example.com/doc.md" {
		t.Fatalf("Partition() docs = %v, want [doc.md]", docs)
	}
	if len(indexes) != 1 || indexes[0] != "https://example.com/llms.txt" {
		t.Fatalf("Partition() indexes = %v, want [llms.txt]", indexes)
	}
	if len(skipped) != 1 || skipped[0].Reason != NonMarkdownReason {
		t.Fatalf("Partition() skipped = %v, want 1 non_markdown", skipped)
	}
}

func TestPartitionAllMarkdown(t *testing.T) {
	links := []string{
		"https://example.com/a.md",
		"https://example.com/b.md",
	}
	docs, indexes, skipped, err := Partition(links)
	if err != nil {
		t.Fatalf("Partition() error = %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Partition() docs = %d, want 2", len(docs))
	}
	if len(indexes) != 0 {
		t.Fatalf("Partition() indexes = %d, want 0", len(indexes))
	}
	if len(skipped) != 0 {
		t.Fatalf("Partition() skipped = %d, want 0", len(skipped))
	}
}

func TestRelativePathNested(t *testing.T) {
	got, err := RelativePath("https://example.com/docs/guide.md", LayoutNested)
	if err != nil {
		t.Fatalf("RelativePath() error = %v", err)
	}
	want := "pages/example.com/docs/guide.md"
	if filepath.ToSlash(got) != want {
		t.Fatalf("RelativePath(nested) = %q, want %q", filepath.ToSlash(got), want)
	}
}

func TestRelativePathRoot(t *testing.T) {
	got, err := RelativePath("https://example.com/docs/guide.md", LayoutRoot)
	if err != nil {
		t.Fatalf("RelativePath() error = %v", err)
	}
	want := "docs/guide.md"
	if filepath.ToSlash(got) != want {
		t.Fatalf("RelativePath(root) = %q, want %q", filepath.ToSlash(got), want)
	}
}

func TestRelativePathQueryParams(t *testing.T) {
	got1, _ := RelativePath("https://example.com/doc.md?v=1", LayoutNested)
	got2, _ := RelativePath("https://example.com/doc.md?v=2", LayoutNested)
	if got1 == got2 {
		t.Fatalf("RelativePath() same for different query params: %q", got1)
	}
}

func TestRelativePathMissingHost(t *testing.T) {
	_, err := RelativePath("not-a-url", LayoutNested)
	if err == nil {
		t.Fatal("RelativePath() expected error for missing host")
	}
}

func TestSourcePath(t *testing.T) {
	if got := SourcePath(LayoutRoot); got != "llms.txt" {
		t.Fatalf("SourcePath(root) = %q, want %q", got, "llms.txt")
	}
	if got := SourcePath(LayoutNested); got == "" {
		t.Fatal("SourcePath(nested) returned empty")
	}
}
