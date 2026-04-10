package fileutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoin joins root and relativePath, returning an error if the result escapes root.
func SafeJoin(root string, relativePath string) (string, error) {
	if root == "" {
		return "", errors.New("missing root directory")
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root directory: %w", err)
	}

	cleanRelative := filepath.Clean(filepath.FromSlash(relativePath))
	if filepath.IsAbs(cleanRelative) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", relativePath)
	}

	targetPath := filepath.Join(absoluteRoot, cleanRelative)
	relativeToRoot, err := filepath.Rel(absoluteRoot, targetPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", relativePath, err)
	}
	if relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes archive root: %q", relativePath)
	}

	return targetPath, nil
}
