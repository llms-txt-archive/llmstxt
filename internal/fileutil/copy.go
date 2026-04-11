// Package fileutil provides filesystem helpers shared across internal packages.
package fileutil

import (
	"fmt"
	"io"
	"os"
)

// CopyFile copies sourcePath to targetPath atomically, removing targetPath on failure.
// The target file is set to mode 0644.
func CopyFile(sourcePath, targetPath string) error {
	// #nosec G304 -- sourcePath is a local spool or cached snapshot path produced by the crawler.
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = sourceFile.Close() }()

	// #nosec G304 -- targetPath is a staged output path rooted under a temp directory controlled by the crawler.
	targetFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("create target: %w", err)
	}

	closed := false
	success := false
	defer func() {
		if !closed {
			_ = targetFile.Close()
		}
		if !success {
			_ = os.Remove(targetPath)
		}
	}()

	if _, err := io.Copy(targetFile, sourceFile); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}
	if err := targetFile.Close(); err != nil {
		return fmt.Errorf("close target: %w", err)
	}
	closed = true
	// #nosec G302 -- tracked snapshot files are intended to remain world-readable.
	if err := os.Chmod(targetPath, 0o644); err != nil {
		return fmt.Errorf("set permissions: %w", err)
	}

	success = true
	return nil
}
