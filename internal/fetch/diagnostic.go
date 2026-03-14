package fetch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"claudecodedocs/internal/fileutil"
	"claudecodedocs/internal/manifest"
)

// BuildFetchFailure creates a FetchFailure record, writing diagnostic files for unexpected content errors.
func BuildFetchFailure(diagnosticsDir string, rawURL string, relativePath string, err error) manifest.FetchFailure {
	failure := manifest.FetchFailure{URL: rawURL, Error: err.Error()}

	var unexpected *UnexpectedContentError
	if errors.As(err, &unexpected) {
		diagnosticPath, diagnosticErr := WriteUnexpectedContentDiagnostic(diagnosticsDir, rawURL, relativePath, unexpected)
		if diagnosticErr != nil {
			failure.Error = fmt.Sprintf("%s (failed to write diagnostic: %v)", failure.Error, diagnosticErr)
		} else {
			failure.DiagnosticPath = diagnosticPath
		}
	}

	return failure
}

// WriteUnexpectedContentDiagnostic saves the response body and metadata of an unexpected content error to diagnosticsDir.
func WriteUnexpectedContentDiagnostic(diagnosticsDir string, rawURL string, relativePath string, unexpected *UnexpectedContentError) (string, error) {
	if diagnosticsDir == "" {
		return "", nil
	}

	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(unexpected.ContentType, ";")[0]))
	bodyExtension := ".txt"
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" || LooksLikeHTMLDocument(unexpected.Sniff) {
		bodyExtension = ".html"
	}

	relativePath = filepath.ToSlash(relativePath)
	bodyRelativePath := relativePath + ".unexpected-content" + bodyExtension
	metaRelativePath := relativePath + ".unexpected-content.json"
	bodyPath := filepath.Join(diagnosticsDir, filepath.FromSlash(bodyRelativePath))
	metaPath := filepath.Join(diagnosticsDir, filepath.FromSlash(metaRelativePath))

	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o750); err != nil {
		return "", fmt.Errorf("create diagnostics directory: %w", err)
	}
	if err := fileutil.CopyFile(unexpected.BodyPath, bodyPath); err != nil {
		return "", fmt.Errorf("write diagnostic body: %w", err)
	}

	metadata := struct {
		URL          string              `json:"url"`
		RelativePath string              `json:"relative_path"`
		Error        string              `json:"error"`
		Status       string              `json:"status,omitempty"`
		ContentType  string              `json:"content_type,omitempty"`
		Headers      map[string][]string `json:"headers,omitempty"`
		BodyPath     string              `json:"body_path"`
	}{
		URL:          rawURL,
		RelativePath: relativePath,
		Error:        unexpected.Message,
		Status:       unexpected.Status,
		ContentType:  unexpected.ContentType,
		Headers:      unexpected.Headers,
		BodyPath:     bodyRelativePath,
	}

	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal diagnostic metadata: %w", err)
	}
	metadataBytes = append(metadataBytes, '\n')

	if err := os.WriteFile(metaPath, metadataBytes, 0o600); err != nil {
		return "", fmt.Errorf("write diagnostic metadata: %w", err)
	}

	return metaRelativePath, nil
}

