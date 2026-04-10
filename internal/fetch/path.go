package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/f-pisani/llmstxt/internal/fileutil"
)

// HashBytes returns the hex-encoded SHA-256 digest of body.
func HashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

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

// SafeJoin joins root and relativePath, returning an error if the result escapes root.
func SafeJoin(root string, relativePath string) (string, error) {
	return fileutil.SafeJoin(root, relativePath)
}

// CleanupSpoolFile removes a temporary spool file, ignoring errors.
func CleanupSpoolFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
