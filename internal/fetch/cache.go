package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/f-pisani/llmstxt/internal/manifest"
)

// SummarizeExistingFile computes the SHA-256 hash and byte count of a file under root.
func SummarizeExistingFile(root string, relativePath string) (localPath string, sha256Value string, bytesCount int64, err error) {
	localPath, err = SafeJoin(root, relativePath)
	if err != nil {
		return "", "", 0, err
	}

	// #nosec G304 -- localPath is anchored to archiveRoot via SafeJoin.
	file, err := os.Open(localPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("read cached file %s: %w", relativePath, err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close cached file %s: %w", relativePath, closeErr)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(hasher, file)
	if err != nil {
		return "", "", 0, fmt.Errorf("hash cached file %s: %w", relativePath, err)
	}

	return localPath, hex.EncodeToString(hasher.Sum(nil)), written, nil
}

// PreservePreviousDocument returns a Result backed by an existing snapshot file when a fresh fetch fails.
func PreservePreviousDocument(archiveRoot string, rawURL string, relativePath string, previous manifest.Entry) (Result, error) {
	if previous.Path == "" {
		return Result{}, ErrNoPreviousEntry
	}

	previousPath := previous.Path

	localPath, sha256Value, bytesCount, err := SummarizeExistingFile(archiveRoot, previousPath)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCachedSummary(previousPath, sha256Value, bytesCount, previous); err != nil {
		return Result{}, err
	}

	return Result{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}, nil
}

// LoadCachedDocument builds a Result from a previously cached file after a 304 Not Modified response.
func LoadCachedDocument(archiveRoot string, rawURL string, relativePath string, previous manifest.Entry, response HTTPResponse) (Result, error) {
	localPath, sha256Value, bytesCount, err := SummarizeExistingFile(archiveRoot, filepath.ToSlash(relativePath))
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCachedSummary(filepath.ToSlash(relativePath), sha256Value, bytesCount, previous); err != nil {
		return Result{}, err
	}

	return Result{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: CoalesceValidator(response.LastModifiedAt, previous.LastModifiedAt),
		ETag:           CoalesceValidator(response.ETag, previous.ETag),
	}, nil
}

// ValidateCachedSummary checks that a cached file's hash and size match the previous manifest entry.
func ValidateCachedSummary(relativePath string, sha256Value string, bytesCount int64, previous manifest.Entry) error {
	if previous.SHA256 != "" && previous.SHA256 != sha256Value {
		return fmt.Errorf("cached file %s does not match previous manifest hash", relativePath)
	}
	if previous.Bytes > 0 && previous.Bytes != bytesCount {
		return fmt.Errorf("cached file %s size does not match previous manifest", relativePath)
	}
	return nil
}
