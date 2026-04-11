package fetch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildFetchFailureRegularError(t *testing.T) {
	failure := BuildFetchFailure("", "https://example.com/doc.md", "doc.md", errors.New("connection refused"))
	if failure.URL != "https://example.com/doc.md" {
		t.Errorf("URL = %q, want %q", failure.URL, "https://example.com/doc.md")
	}
	if failure.Error != "connection refused" {
		t.Errorf("Error = %q, want %q", failure.Error, "connection refused")
	}
	if failure.DiagnosticPath != "" {
		t.Errorf("DiagnosticPath = %q, want empty", failure.DiagnosticPath)
	}
}

func TestBuildFetchFailureUnexpectedContentError(t *testing.T) {
	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.html")
	if err := os.WriteFile(bodyFile, []byte("<html>error page</html>"), 0o600); err != nil {
		t.Fatal(err)
	}

	uce := &UnexpectedContentError{
		Message:     "expected markdown response but received text/html",
		Status:      "200 OK",
		ContentType: "text/html",
		Headers:     map[string][]string{"Content-Type": {"text/html"}},
		Sniff:       []byte("<html>error page</html>"),
		BodyPath:    bodyFile,
	}

	diagnosticsDir := filepath.Join(dir, "diagnostics")
	failure := BuildFetchFailure(diagnosticsDir, "https://example.com/doc.md", "doc.md", uce)
	if failure.DiagnosticPath == "" {
		t.Fatal("expected DiagnosticPath to be set for UnexpectedContentError")
	}
}

func TestBuildFetchFailureUnexpectedContentNoDiagDir(t *testing.T) {
	uce := &UnexpectedContentError{
		Message:     "expected markdown",
		ContentType: "text/html",
	}

	// Empty diagnostics dir should result in empty diagnostic path (no error).
	failure := BuildFetchFailure("", "https://example.com/doc.md", "doc.md", uce)
	if failure.DiagnosticPath != "" {
		t.Errorf("DiagnosticPath = %q, want empty when diagnosticsDir is empty", failure.DiagnosticPath)
	}
}

func TestWriteUnexpectedContentDiagnosticEmptyDir(t *testing.T) {
	uce := &UnexpectedContentError{
		Message:     "test error",
		ContentType: "text/html",
	}

	path, err := WriteUnexpectedContentDiagnostic("", "https://example.com/doc.md", "doc.md", uce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty for empty diagnosticsDir", path)
	}
}

func TestWriteUnexpectedContentDiagnosticValidDir(t *testing.T) {
	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.html")
	if err := os.WriteFile(bodyFile, []byte("<html>error</html>"), 0o600); err != nil {
		t.Fatal(err)
	}

	uce := &UnexpectedContentError{
		Message:     "expected markdown response but received text/html",
		Status:      "200 OK",
		ContentType: "text/html",
		Headers:     map[string][]string{"Content-Type": {"text/html"}},
		Sniff:       []byte("<html>error</html>"),
		BodyPath:    bodyFile,
	}

	diagnosticsDir := filepath.Join(dir, "diagnostics")
	metaRelPath, err := WriteUnexpectedContentDiagnostic(diagnosticsDir, "https://example.com/doc.md", "doc.md", uce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metaRelPath == "" {
		t.Fatal("expected non-empty diagnostic path")
	}

	// Verify files were created.
	metaPath := filepath.Join(diagnosticsDir, filepath.FromSlash(metaRelPath))
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("metadata file not found: %v", err)
	}

	// Body file should have .html extension since content-type is text/html.
	bodyDiagPath := filepath.Join(diagnosticsDir, "doc.md.unexpected-content.html")
	if _, err := os.Stat(bodyDiagPath); err != nil {
		t.Fatalf("body diagnostic file not found: %v", err)
	}
}

func TestWriteUnexpectedContentDiagnosticTextExtension(t *testing.T) {
	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(bodyFile, []byte("some plain text error"), 0o600); err != nil {
		t.Fatal(err)
	}

	uce := &UnexpectedContentError{
		Message:     "some error",
		Status:      "200 OK",
		ContentType: "application/json",
		Sniff:       []byte("some plain text error"),
		BodyPath:    bodyFile,
	}

	diagnosticsDir := filepath.Join(dir, "diagnostics")
	_, err := WriteUnexpectedContentDiagnostic(diagnosticsDir, "https://example.com/doc.md", "doc.md", uce)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Non-HTML content should get .txt extension.
	bodyDiagPath := filepath.Join(diagnosticsDir, "doc.md.unexpected-content.txt")
	if _, err := os.Stat(bodyDiagPath); err != nil {
		t.Fatalf("body diagnostic file not found (expected .txt extension): %v", err)
	}
}
