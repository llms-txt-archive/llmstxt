package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m := &Manifest{
		SourceURL:     "https://example.com/llms.txt",
		DocumentCount: 2,
		Documents: []Entry{
			{URL: "https://example.com/a.md", Path: "a.md", SHA256: "abc", Bytes: 100},
		},
	}
	if err := Write(path, m); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.SourceURL != m.SourceURL {
		t.Fatalf("Load() source_url = %q, want %q", got.SourceURL, m.SourceURL)
	}
	if len(got.Documents) != 1 {
		t.Fatalf("Load() documents = %d, want 1", len(got.Documents))
	}
}

func TestLoadEmptyPath(t *testing.T) {
	got, err := Load("")
	if err != nil || got != nil {
		t.Fatalf("Load(\"\") = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || got != nil {
		t.Fatalf("Load(missing) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{invalid"), 0o600)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(invalid) expected error")
	}
}

func TestWriteAndLoadSetsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m := &Manifest{SourceURL: "https://example.com/llms.txt"}
	if err := Write(path, m); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Version != ManifestVersion {
		t.Fatalf("Load() version = %d, want %d", got.Version, ManifestVersion)
	}
}

func TestLoadUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	os.WriteFile(path, []byte(`{"version":99,"source_url":"https://example.com/llms.txt"}`), 0o600)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for unsupported version")
	}
}

func TestLoadVersionZeroBackwardsCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	os.WriteFile(path, []byte(`{"source_url":"https://example.com/llms.txt"}`), 0o600)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Version != 0 {
		t.Fatalf("Load() version = %d, want 0", got.Version)
	}
}

func TestPreviousDocumentsByURL(t *testing.T) {
	m := &Manifest{
		Documents: []Entry{
			{URL: "https://example.com/a.md", Path: "a.md"},
			{URL: "https://example.com/b.md", Path: "b.md"},
		},
	}
	docs := PreviousDocumentsByURL(m)
	if len(docs) != 2 {
		t.Fatalf("PreviousDocumentsByURL() = %d entries, want 2", len(docs))
	}
	if docs["https://example.com/a.md"].Path != "a.md" {
		t.Fatal("PreviousDocumentsByURL() missing expected entry")
	}
}

func TestPreviousDocumentsByURLNil(t *testing.T) {
	docs := PreviousDocumentsByURL(nil)
	if docs != nil {
		t.Fatalf("PreviousDocumentsByURL(nil) = %v, want nil", docs)
	}
}

func TestPreviousSourceEntry(t *testing.T) {
	m := &Manifest{
		SourceURL:    "https://example.com/llms.txt",
		SourcePath:   "source/llms.txt",
		SourceSHA256: "abc123",
	}
	entry := PreviousSourceEntry(m, "fallback.txt")
	if entry.Path != "source/llms.txt" {
		t.Fatalf("PreviousSourceEntry() path = %q, want %q", entry.Path, "source/llms.txt")
	}
}

func TestPreviousSourceEntryNil(t *testing.T) {
	entry := PreviousSourceEntry(nil, "fallback.txt")
	if entry.Path != "fallback.txt" {
		t.Fatalf("PreviousSourceEntry(nil) path = %q, want %q", entry.Path, "fallback.txt")
	}
}
