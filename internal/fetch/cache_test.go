package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/llms-txt-archive/llmstxt/internal/manifest"
)

func TestSummarizeExistingFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("test content for hashing")
	filePath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	expectedHash := sha256.Sum256(content)
	expectedHex := hex.EncodeToString(expectedHash[:])

	localPath, sha256Value, bytesCount, err := SummarizeExistingFile(dir, "doc.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if localPath != filePath {
		t.Errorf("localPath = %q, want %q", localPath, filePath)
	}
	if sha256Value != expectedHex {
		t.Errorf("sha256 = %q, want %q", sha256Value, expectedHex)
	}
	if bytesCount != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", bytesCount, len(content))
	}
}

func TestSummarizeExistingFileMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, _, err := SummarizeExistingFile(dir, "nonexistent.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestPreservePreviousDocumentHappyPath(t *testing.T) {
	dir := t.TempDir()
	content := []byte("preserved document")
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	previous := manifest.Entry{
		Path:           "doc.md",
		SHA256:         hashHex,
		Bytes:          int64(len(content)),
		LastModifiedAt: "2025-01-01T00:00:00Z",
		ETag:           `"abc123"`,
	}

	result, err := PreservePreviousDocument(dir, "https://example.com/doc.md", "doc.md", previous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL != "https://example.com/doc.md" {
		t.Errorf("URL = %q, want %q", result.URL, "https://example.com/doc.md")
	}
	if result.SHA256 != hashHex {
		t.Errorf("SHA256 = %q, want %q", result.SHA256, hashHex)
	}
	if result.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", result.Bytes, len(content))
	}
	if result.LastModifiedAt != previous.LastModifiedAt {
		t.Errorf("LastModifiedAt = %q, want %q", result.LastModifiedAt, previous.LastModifiedAt)
	}
	if result.ETag != previous.ETag {
		t.Errorf("ETag = %q, want %q", result.ETag, previous.ETag)
	}
}

func TestPreservePreviousDocumentNoPreviousEntry(t *testing.T) {
	dir := t.TempDir()
	_, err := PreservePreviousDocument(dir, "https://example.com/doc.md", "doc.md", manifest.Entry{})
	if !errors.Is(err, ErrNoPreviousEntry) {
		t.Fatalf("expected ErrNoPreviousEntry, got %v", err)
	}
}

func TestPreservePreviousDocumentHashMismatch(t *testing.T) {
	dir := t.TempDir()
	content := []byte("actual content")
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	previous := manifest.Entry{
		Path:   "doc.md",
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		Bytes:  int64(len(content)),
	}

	_, err := PreservePreviousDocument(dir, "https://example.com/doc.md", "doc.md", previous)
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
}

func TestLoadCachedDocumentHappyPath(t *testing.T) {
	dir := t.TempDir()
	content := []byte("cached document")
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	previous := manifest.Entry{
		Path:           "doc.md",
		SHA256:         hashHex,
		Bytes:          int64(len(content)),
		LastModifiedAt: "2025-01-01T00:00:00Z",
		ETag:           `"old-etag"`,
	}
	response := HTTPResponse{
		LastModifiedAt: "2025-06-01T00:00:00Z",
		ETag:           `"new-etag"`,
	}

	result, err := LoadCachedDocument(dir, "https://example.com/doc.md", "doc.md", previous, response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SHA256 != hashHex {
		t.Errorf("SHA256 = %q, want %q", result.SHA256, hashHex)
	}
	if result.LastModifiedAt != "2025-06-01T00:00:00Z" {
		t.Errorf("LastModifiedAt = %q, want response value", result.LastModifiedAt)
	}
	if result.ETag != `"new-etag"` {
		t.Errorf("ETag = %q, want response value", result.ETag)
	}
}

func TestLoadCachedDocumentFallbackValidators(t *testing.T) {
	dir := t.TempDir()
	content := []byte("cached document")
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	previous := manifest.Entry{
		Path:           "doc.md",
		SHA256:         hashHex,
		Bytes:          int64(len(content)),
		LastModifiedAt: "2025-01-01T00:00:00Z",
		ETag:           `"old-etag"`,
	}
	response := HTTPResponse{} // empty validators

	result, err := LoadCachedDocument(dir, "https://example.com/doc.md", "doc.md", previous, response)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LastModifiedAt != "2025-01-01T00:00:00Z" {
		t.Errorf("LastModifiedAt = %q, want fallback to previous", result.LastModifiedAt)
	}
	if result.ETag != `"old-etag"` {
		t.Errorf("ETag = %q, want fallback to previous", result.ETag)
	}
}

func TestValidateCachedSummaryPass(t *testing.T) {
	err := ValidateCachedSummary("doc.md", "abc123", 100, manifest.Entry{
		SHA256: "abc123",
		Bytes:  100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateCachedSummaryEmptyPrevious(t *testing.T) {
	// Empty previous values should pass (no constraint to check).
	err := ValidateCachedSummary("doc.md", "abc123", 100, manifest.Entry{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateCachedSummaryHashMismatch(t *testing.T) {
	err := ValidateCachedSummary("doc.md", "actual-hash", 100, manifest.Entry{
		SHA256: "expected-hash",
		Bytes:  100,
	})
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
}

func TestValidateCachedSummarySizeMismatch(t *testing.T) {
	err := ValidateCachedSummary("doc.md", "abc123", 200, manifest.Entry{
		SHA256: "abc123",
		Bytes:  100,
	})
	if err == nil {
		t.Fatal("expected error for size mismatch")
	}
}
