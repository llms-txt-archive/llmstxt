// Package stage atomically replaces the output directory with freshly fetched documents.
package stage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"claudecodedocs/internal/fetch"
	"claudecodedocs/internal/fileutil"
)

const (
	journalName  = ".claudecodedocs-stage.json"
	completeName = ".claudecodedocs-complete"
)

// StageOptions configures staging behavior. A nil *StageOptions is valid and uses defaults.
type StageOptions struct {
	// RemoveAll overrides the directory removal function. Defaults to os.RemoveAll if nil.
	RemoveAll func(string) error
}

func (o *StageOptions) removeAll() func(string) error {
	if o != nil && o.RemoveAll != nil {
		return o.RemoveAll
	}
	return os.RemoveAll
}

// Journal records the in-progress state of an atomic directory replacement for crash recovery.
type Journal struct {
	TempDir   string `json:"temp_dir"`
	OutputDir string `json:"output_dir"`
	BackupDir string `json:"backup_dir"`
	Phase     string `json:"phase"`
}

// StageOutput writes fetched documents to a temporary directory and atomically swaps it into outputDir.
func StageOutput(outputDir string, source fetch.Result, documents []fetch.Result, opts *StageOptions) error {
	parentDir := filepath.Dir(outputDir)
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	removeAll := opts.removeAll()
	if err := ReconcileState(outputDir, opts); err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp(parentDir, ".claudecodedocs-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}

	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = removeAll(tempDir)
		}
	}()

	if err := writeResult(tempDir, source); err != nil {
		return err
	}
	for _, document := range documents {
		if err := writeResult(tempDir, document); err != nil {
			return err
		}
	}
	if err := WriteCompletionMarker(tempDir); err != nil {
		return err
	}
	if err := ReplaceDir(tempDir, outputDir, opts); err != nil {
		return err
	}

	cleanupTemp = false
	return nil
}

func writeResult(root string, result fetch.Result) error {
	targetPath := filepath.Join(root, filepath.FromSlash(result.RelativePath))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return fmt.Errorf("create directory for %s: %w", targetPath, err)
	}

	if err := fileutil.CopyFile(result.LocalPath, targetPath); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	return nil
}

// ReplaceDir atomically replaces outputDir with tempDir using a backup-and-rename strategy.
func ReplaceDir(tempDir string, outputDir string, opts *StageOptions) error {
	removeAll := opts.removeAll()
	backupDir := outputDir + ".bak"
	journal := Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: backupDir,
		Phase:     "staged",
	}
	if err := WriteJournal(outputDir, journal); err != nil {
		return err
	}

	outputExists := pathExists(outputDir)
	backupExists := pathExists(backupDir)

	if backupExists && !outputExists {
		if err := os.Rename(backupDir, outputDir); err != nil {
			return fmt.Errorf("restore backup output: %w", err)
		}
		outputExists = true
		backupExists = false
	}
	if backupExists && outputExists {
		if err := removeAll(backupDir); err != nil {
			return fmt.Errorf("remove stale backup directory: %w", err)
		}
		backupExists = false
	}

	if outputExists {
		journal.Phase = "backup_existing_output"
		if err := WriteJournal(outputDir, journal); err != nil {
			return err
		}
		if err := os.Rename(outputDir, backupDir); err != nil {
			return fmt.Errorf("backup existing output: %w", err)
		}
		backupExists = true
		journal.Phase = "backup_created"
		if err := WriteJournal(outputDir, journal); err != nil {
			return err
		}
	}

	journal.Phase = "activate_output"
	if err := WriteJournal(outputDir, journal); err != nil {
		return err
	}
	if err := os.Rename(tempDir, outputDir); err != nil {
		if backupExists {
			if restoreErr := os.Rename(backupDir, outputDir); restoreErr != nil {
				return fmt.Errorf("activate new output: %w (restore backup: %w)", err, restoreErr)
			}
		}
		return fmt.Errorf("activate new output: %w", err)
	}
	if err := RemoveCompletionMarker(outputDir); err != nil {
		return err
	}

	if backupExists {
		if err := removeAll(backupDir); err != nil {
			slog.Warn("failed to remove backup directory", "path", backupDir, "error", err)
		}
	}
	if err := RemoveJournal(outputDir); err != nil {
		slog.Warn("failed to remove stage journal", "path", outputDir, "error", err)
	}

	return nil
}

// ReconcileState recovers from a previously interrupted staging operation by replaying the journal.
func ReconcileState(outputDir string, opts *StageOptions) error {
	removeAll := opts.removeAll()
	backupDir := outputDir + ".bak"
	outputExists := pathExists(outputDir)
	backupExists := pathExists(backupDir)

	journal, err := LoadJournal(outputDir)
	if err != nil {
		return err
	}
	if journal == nil {
		if outputExists && backupExists {
			if err := removeAll(backupDir); err != nil {
				return fmt.Errorf("remove stale backup directory: %w", err)
			}
			return nil
		}
		if !outputExists && backupExists {
			if err := os.Rename(backupDir, outputDir); err != nil {
				return fmt.Errorf("restore backup output: %w", err)
			}
		}
		return nil
	}

	outputExists = pathExists(journal.OutputDir)
	backupExists = pathExists(journal.BackupDir)
	tempExists := pathExists(journal.TempDir)
	markerExists := pathExists(CompletionMarkerPath(journal.TempDir))

	switch {
	case outputExists:
		if backupExists {
			if err := removeAll(journal.BackupDir); err != nil {
				return fmt.Errorf("remove stale backup directory: %w", err)
			}
		}
		if tempExists {
			if err := removeAll(journal.TempDir); err != nil {
				return fmt.Errorf("remove stale staged directory: %w", err)
			}
		}
	case backupExists:
		if err := os.Rename(journal.BackupDir, journal.OutputDir); err != nil {
			return fmt.Errorf("restore backup output: %w", err)
		}
		if tempExists {
			if err := removeAll(journal.TempDir); err != nil {
				return fmt.Errorf("remove stale staged directory: %w", err)
			}
		}
	case tempExists && markerExists:
		if err := os.Rename(journal.TempDir, journal.OutputDir); err != nil {
			return fmt.Errorf("promote recovered staged output: %w", err)
		}
		if err := RemoveCompletionMarker(journal.OutputDir); err != nil {
			return err
		}
	case tempExists:
		if err := removeAll(journal.TempDir); err != nil {
			return fmt.Errorf("remove incomplete staged directory: %w", err)
		}
	}

	if err := RemoveJournal(outputDir); err != nil {
		return err
	}
	return nil
}

// JournalPath returns the path to the staging journal file for the given output directory.
func JournalPath(outputDir string) string {
	return filepath.Join(filepath.Dir(outputDir), journalName)
}

// CompletionMarkerPath returns the path to the completion marker file within root.
func CompletionMarkerPath(root string) string {
	return filepath.Join(root, completeName)
}

// WriteCompletionMarker creates a marker file indicating that all documents have been written to root.
func WriteCompletionMarker(root string) error {
	if err := os.WriteFile(CompletionMarkerPath(root), []byte("complete\n"), 0o600); err != nil {
		return fmt.Errorf("write stage completion marker: %w", err)
	}
	return nil
}

// RemoveCompletionMarker deletes the completion marker file from root.
func RemoveCompletionMarker(root string) error {
	if err := os.Remove(CompletionMarkerPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stage completion marker: %w", err)
	}
	return nil
}

// WriteJournal persists the staging journal to disk for crash recovery.
func WriteJournal(outputDir string, journal Journal) error {
	journalPath := JournalPath(outputDir)
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stage journal: %w", err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(journalPath, body, 0o600); err != nil {
		return fmt.Errorf("write stage journal: %w", err)
	}
	return nil
}

// LoadJournal reads and parses a previously written staging journal, returning nil if none exists.
func LoadJournal(outputDir string) (*Journal, error) {
	journalPath := JournalPath(outputDir)
	// #nosec G304 -- journalPath is anchored beside the managed output directory.
	body, err := os.ReadFile(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stage journal: %w", err)
	}

	var journal Journal
	if err := json.Unmarshal(body, &journal); err != nil {
		return nil, fmt.Errorf("parse stage journal: %w", err)
	}
	return &journal, nil
}

// RemoveJournal deletes the staging journal file for the given output directory.
func RemoveJournal(outputDir string) error {
	if err := os.Remove(JournalPath(outputDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stage journal: %w", err)
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
