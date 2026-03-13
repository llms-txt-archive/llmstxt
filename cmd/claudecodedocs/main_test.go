package main

import (
	"reflect"
	"testing"
)

func TestExtractLinksDeduplicatesAndSorts(t *testing.T) {
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

func TestRelativePathForURL(t *testing.T) {
	got, err := relativePathForURL("https://code.claude.com/docs/en/overview.md")
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := "pages/code.claude.com/docs/en/overview.md"
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
	}
}

func TestRelativePathForURLWithQuery(t *testing.T) {
	got, err := relativePathForURL("https://example.com/docs/page?lang=en")
	if err != nil {
		t.Fatalf("relativePathForURL() error = %v", err)
	}

	want := "pages/example.com/docs/page__0933497c2075.html"
	if got != want {
		t.Fatalf("relativePathForURL() = %q, want %q", got, want)
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
