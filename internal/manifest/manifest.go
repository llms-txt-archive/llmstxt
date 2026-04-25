// Package manifest reads and writes the JSON manifest that records sync results.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// normalizeSourceURL strips the fragment from a URL for consistent map lookups.
func normalizeSourceURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.Fragment = ""
	return parsed.String()
}

// SourceEntry records a nested llms.txt index discovered during recursive crawling.
type SourceEntry struct {
	URL            string `json:"url"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256,omitempty"`
	LastModifiedAt string `json:"last_modified_at,omitempty"`
	ETag           string `json:"etag,omitempty"`
}

// ManifestVersion is the current manifest schema version.
const ManifestVersion = 1

// Manifest describes the result of a sync run, including all fetched documents, skipped URLs, and failures.
type Manifest struct {
	Version              int            `json:"version"`
	SourceURL            string         `json:"source_url"`
	SourcePath           string         `json:"source_path"`
	SourceSHA256         string         `json:"source_sha256,omitempty"`
	SourceLastModifiedAt string         `json:"source_last_modified_at,omitempty"`
	SourceETag           string         `json:"source_etag,omitempty"`
	DocumentCount        int            `json:"document_count"`
	SkippedCount         int            `json:"skipped_count,omitempty"`
	Documents            []Entry        `json:"documents,omitempty"`
	Skipped              []SkippedEntry `json:"skipped,omitempty"`
	Failures             []FetchFailure `json:"failures,omitempty"`
	Sources              []SourceEntry  `json:"sources,omitempty"`
}

// Entry records the URL, local path, and integrity metadata for a single fetched document.
type Entry struct {
	URL            string `json:"url"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	Bytes          int64  `json:"bytes"`
	LastModifiedAt string `json:"last_modified_at,omitempty"`
	ETag           string `json:"etag,omitempty"`
}

// SkippedEntry records a URL that was found in llms.txt but not fetched, along with the reason.
type SkippedEntry struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// FetchFailure records a URL that could not be fetched and the associated error.
type FetchFailure struct {
	URL               string `json:"url"`
	Error             string `json:"error"`
	DiagnosticPath    string `json:"diagnostic_path,omitempty"`
	PreservedExisting bool   `json:"preserved_existing,omitempty"`
}

// Load reads and parses a manifest JSON file, returning nil if the path is empty or the file does not exist.
func Load(manifestPath string) (*Manifest, error) {
	if manifestPath == "" {
		return nil, nil
	}

	// #nosec G304 -- manifestPath is a local CLI input to a release asset on disk.
	body, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifestData Manifest
	if err := json.Unmarshal(body, &manifestData); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	if manifestData.Version != 0 && manifestData.Version != ManifestVersion {
		return nil, fmt.Errorf("unsupported manifest version %d (expected %d)", manifestData.Version, ManifestVersion)
	}

	return &manifestData, nil
}

// Write serializes manifestData as indented JSON and writes it to manifestPath.
func Write(manifestPath string, manifestData *Manifest) error {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}

	manifestData.Version = ManifestVersion

	manifestBytes, err := json.MarshalIndent(manifestData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}

// PreviousDocumentsByURL indexes the documents of a previous manifest by URL for conditional-fetch lookups.
func PreviousDocumentsByURL(manifestData *Manifest) map[string]Entry {
	if manifestData == nil {
		return nil
	}

	documents := make(map[string]Entry, len(manifestData.Documents))
	for _, document := range manifestData.Documents {
		documents[document.URL] = document
	}

	return documents
}

// PreviousSourceDocURLs builds a map from each nested index URL to the doc URLs
// that were discovered through it in the previous run. This allows the caller to
// preserve previously known docs when a nested index is temporarily unreachable.
// The mapping is approximate: it maps every document to the primary source and
// every source to its own URL. A future manifest version could record this
// relationship explicitly.
func PreviousSourceDocURLs(manifestData *Manifest) map[string][]string {
	if manifestData == nil || len(manifestData.Sources) == 0 {
		return nil
	}

	// Without per-index provenance tracking, we cannot know which docs came
	// from which nested index. Return all doc URLs for every source so that
	// a failed index at least triggers preservation of previously known docs.
	allDocURLs := make([]string, 0, len(manifestData.Documents))
	for _, doc := range manifestData.Documents {
		allDocURLs = append(allDocURLs, doc.URL)
	}

	result := make(map[string][]string, len(manifestData.Sources))
	for _, src := range manifestData.Sources {
		result[normalizeSourceURL(src.URL)] = allDocURLs
	}
	return result
}

// PreviousSourceEntry returns the source entry from a previous manifest, using fallbackPath if no path is recorded.
func PreviousSourceEntry(manifestData *Manifest, fallbackPath string) Entry {
	if manifestData == nil {
		return Entry{Path: fallbackPath}
	}

	sourcePath := manifestData.SourcePath
	if sourcePath == "" {
		sourcePath = fallbackPath
	}

	return Entry{
		URL:            manifestData.SourceURL,
		Path:           sourcePath,
		SHA256:         manifestData.SourceSHA256,
		LastModifiedAt: manifestData.SourceLastModifiedAt,
		ETag:           manifestData.SourceETag,
	}
}
