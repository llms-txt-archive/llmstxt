package stage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ---------- FileEntry ----------

func TestFileEntry(t *testing.T) {
	fe := FileEntry{RelativePath: "docs/intro.md", LocalPath: "/tmp/intro.md"}
	if fe.RelativePath != "docs/intro.md" {
		t.Fatalf("unexpected RelativePath: %s", fe.RelativePath)
	}
	if fe.LocalPath != "/tmp/intro.md" {
		t.Fatalf("unexpected LocalPath: %s", fe.LocalPath)
	}
}

// ---------- JournalPath / CompletionMarkerPath ----------

func TestJournalPath(t *testing.T) {
	got := JournalPath("/data/output")
	want := filepath.Join("/data", journalName)
	if got != want {
		t.Fatalf("JournalPath = %q, want %q", got, want)
	}
}

func TestCompletionMarkerPath(t *testing.T) {
	got := CompletionMarkerPath("/data/output")
	want := filepath.Join("/data/output", completeName)
	if got != want {
		t.Fatalf("CompletionMarkerPath = %q, want %q", got, want)
	}
}

// ---------- WriteCompletionMarker / RemoveCompletionMarker ----------

func TestCompletionMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if err := WriteCompletionMarker(dir); err != nil {
		t.Fatalf("WriteCompletionMarker: %v", err)
	}
	markerPath := CompletionMarkerPath(dir)
	data, err := os.ReadFile(markerPath) // #nosec G304 -- test reads temp files it created
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(data) != "complete\n" {
		t.Fatalf("marker content = %q, want %q", data, "complete\n")
	}

	if err := RemoveCompletionMarker(dir); err != nil {
		t.Fatalf("RemoveCompletionMarker: %v", err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker should be removed, got err: %v", err)
	}
}

func TestRemoveCompletionMarkerMissing(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveCompletionMarker(dir); err != nil {
		t.Fatalf("RemoveCompletionMarker on missing file should not error: %v", err)
	}
}

// ---------- WriteJournal / LoadJournal / RemoveJournal ----------

func TestJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")

	journal := Journal{
		TempDir:   "/tmp/staged",
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     phaseStaged,
	}

	if err := WriteJournal(outputDir, journal); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}

	loaded, err := LoadJournal(outputDir)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadJournal returned nil")
	}
	if *loaded != journal {
		t.Fatalf("loaded journal = %+v, want %+v", *loaded, journal)
	}

	if err := RemoveJournal(outputDir); err != nil {
		t.Fatalf("RemoveJournal: %v", err)
	}

	loaded, err = LoadJournal(outputDir)
	if err != nil {
		t.Fatalf("LoadJournal after remove: %v", err)
	}
	if loaded != nil {
		t.Fatal("LoadJournal should return nil after removal")
	}
}

func TestLoadJournalMissing(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "nonexistent")

	loaded, err := LoadJournal(outputDir)
	if err != nil {
		t.Fatalf("LoadJournal on missing file: %v", err)
	}
	if loaded != nil {
		t.Fatal("expected nil journal for missing file")
	}
}

func TestLoadJournalCorrupt(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	journalPath := JournalPath(outputDir)

	if err := os.WriteFile(journalPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadJournal(outputDir)
	if err == nil {
		t.Fatal("expected error for corrupt journal")
	}
}

func TestRemoveJournalMissing(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "nonexistent")

	if err := RemoveJournal(outputDir); err != nil {
		t.Fatalf("RemoveJournal on missing file should not error: %v", err)
	}
}

func TestWriteJournalContent(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")

	journal := Journal{
		TempDir:   "/tmp/staged",
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     phaseBackupCreated,
	}

	if err := WriteJournal(outputDir, journal); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(JournalPath(outputDir))
	if err != nil {
		t.Fatal(err)
	}

	var parsed Journal
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("journal is not valid JSON: %v", err)
	}
	if parsed.Phase != phaseBackupCreated {
		t.Fatalf("phase = %q, want %q", parsed.Phase, phaseBackupCreated)
	}
}

// ---------- pathExists ----------

func TestPathExistsExisting(t *testing.T) {
	dir := t.TempDir()
	exists, err := pathExists(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected true for existing directory")
	}
}

func TestPathExistsNonExisting(t *testing.T) {
	exists, err := pathExists(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected false for non-existing path")
	}
}

func TestPathExistsPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission test not reliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks skipped")
	}

	dir := t.TempDir()
	child := filepath.Join(dir, "restricted")
	if err := os.Mkdir(child, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(child, "file.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Remove read+execute from the parent so Stat on the child fails.
	if err := os.Chmod(child, 0o000); err != nil { // #nosec G302 -- test deliberately sets permissions for error path testing
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(child, 0o750) }) //nolint:gosec // G302 -- restoring permissions in test cleanup

	_, err := pathExists(target)
	if err == nil {
		t.Fatal("expected error for permission-denied path")
	}
}

// ---------- writeFile ----------

func TestWriteFileHappyPath(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "source.md")
	content := "# Hello\n"
	if err := os.WriteFile(srcFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	fe := FileEntry{RelativePath: "docs/source.md", LocalPath: srcFile}
	if err := writeFile(root, fe); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "docs", "source.md")) // #nosec G304
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestWriteFilePathTraversal(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "evil.md")
	if err := os.WriteFile(srcFile, []byte("pwned"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	fe := FileEntry{RelativePath: "../escape.md", LocalPath: srcFile}
	err := writeFile(root, fe)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

// ---------- Output ----------

func TestOutputHappyPath(t *testing.T) {
	base := t.TempDir()

	// Create source files.
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatal(err)
	}
	files := []FileEntry{
		{RelativePath: "intro.md", LocalPath: filepath.Join(srcDir, "intro.md")},
		{RelativePath: "guide/setup.md", LocalPath: filepath.Join(srcDir, "setup.md")},
	}
	if err := os.WriteFile(files[0].LocalPath, []byte("# Intro\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[1].LocalPath, []byte("# Setup\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(base, "output")
	if err := Output(outputDir, files, nil); err != nil {
		t.Fatalf("Output: %v", err)
	}

	// Verify files exist in output.
	for _, f := range files {
		p := filepath.Join(outputDir, f.RelativePath)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s to exist: %v", f.RelativePath, err)
		}
	}

	// Completion marker should be removed after successful Output.
	if _, err := os.Stat(CompletionMarkerPath(outputDir)); !os.IsNotExist(err) {
		t.Error("completion marker should be removed after Output")
	}

	// Journal should be cleaned up.
	if _, err := os.Stat(JournalPath(outputDir)); !os.IsNotExist(err) {
		t.Error("journal should be removed after Output")
	}
}

func TestOutputPathTraversal(t *testing.T) {
	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "evil.md")
	if err := os.WriteFile(srcFile, []byte("pwned"), 0o600); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(base, "output")
	files := []FileEntry{
		{RelativePath: "../../etc/shadow", LocalPath: srcFile},
	}
	err := Output(outputDir, files, nil)
	if err == nil {
		t.Fatal("expected error for path traversal in Output")
	}
}

func TestOutputReplacesExistingDir(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")

	// Create an existing output directory with a file.
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "old.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create source for new output.
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "new.md")
	if err := os.WriteFile(srcFile, []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}

	files := []FileEntry{{RelativePath: "new.md", LocalPath: srcFile}}
	if err := Output(outputDir, files, nil); err != nil {
		t.Fatalf("Output: %v", err)
	}

	// New file should exist.
	if _, err := os.Stat(filepath.Join(outputDir, "new.md")); err != nil {
		t.Error("new.md should exist after Output")
	}
	// Old file should be gone (atomic replace).
	if _, err := os.Stat(filepath.Join(outputDir, "old.md")); !os.IsNotExist(err) {
		t.Error("old.md should not exist after atomic replace")
	}
	// Backup should be cleaned up.
	if _, err := os.Stat(outputDir + ".bak"); !os.IsNotExist(err) {
		t.Error("backup directory should be cleaned up")
	}
}

// ---------- ReplaceDir ----------

func TestReplaceDirAtomicSwap(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	tempDir := filepath.Join(base, "temp")

	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "file.md"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCompletionMarker(tempDir); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceDir(tempDir, outputDir, nil); err != nil {
		t.Fatalf("ReplaceDir: %v", err)
	}

	// tempDir should no longer exist (it was renamed).
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Error("tempDir should not exist after ReplaceDir")
	}
	// outputDir should have the file.
	data, err := os.ReadFile(filepath.Join(outputDir, "file.md")) // #nosec G304
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" {
		t.Fatalf("content = %q, want %q", data, "data")
	}
}

func TestReplaceDirLeftoverBackup(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(base, "temp")

	// Create existing output and a leftover backup.
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "stale.md"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create temp with new content.
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "fresh.md"), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCompletionMarker(tempDir); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceDir(tempDir, outputDir, nil); err != nil {
		t.Fatalf("ReplaceDir with leftover backup: %v", err)
	}

	// Backup should be cleaned up.
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Error("backup directory should be cleaned up")
	}
	// New output should be in place.
	if _, err := os.Stat(filepath.Join(outputDir, "fresh.md")); err != nil {
		t.Error("fresh.md should exist in output")
	}
}

// ---------- ReconcileState ----------

func TestReconcileStateClean(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")

	// No journal, no output, no backup — should be a no-op.
	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState clean: %v", err)
	}
}

func TestReconcileStateNoJournalWithBackupOnly(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	backupDir := outputDir + ".bak"

	// No journal but backup exists without output — should restore backup.
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "restored.md"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Output should now exist (restored from backup).
	if _, err := os.Stat(filepath.Join(outputDir, "restored.md")); err != nil {
		t.Error("restored.md should exist in output after reconcile")
	}
	// Backup should be gone.
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Error("backup should not exist after restore")
	}
}

func TestReconcileStateNoJournalWithOutputAndBackup(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	backupDir := outputDir + ".bak"

	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Output stays, backup removed.
	if _, err := os.Stat(outputDir); err != nil {
		t.Error("output should still exist")
	}
	if _, err := os.Stat(backupDir); !os.IsNotExist(err) {
		t.Error("backup should be removed")
	}
}

func TestReconcileStateJournalWithBackup(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	backupDir := outputDir + ".bak"
	tempDir := filepath.Join(base, "temp-staged")

	// Simulate crash: backup exists, no output, journal present.
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "backup.md"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	journal := Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: backupDir,
		Phase:     phaseBackupCreated,
	}
	if err := WriteJournal(outputDir, journal); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Output should be restored from backup.
	if _, err := os.Stat(filepath.Join(outputDir, "backup.md")); err != nil {
		t.Error("backup.md should be restored to output")
	}
	// Journal should be cleaned up.
	if _, err := os.Stat(JournalPath(outputDir)); !os.IsNotExist(err) {
		t.Error("journal should be removed after reconcile")
	}
}

func TestReconcileStateJournalWithTempAndCompletionMarker(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	tempDir := filepath.Join(base, "temp-staged")

	// Simulate crash: temp with completion marker, no output, no backup.
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "promoted.md"), []byte("promoted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCompletionMarker(tempDir); err != nil {
		t.Fatal(err)
	}

	journal := Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     phaseActivate,
	}
	if err := WriteJournal(outputDir, journal); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Temp should be promoted to output.
	if _, err := os.Stat(filepath.Join(outputDir, "promoted.md")); err != nil {
		t.Error("promoted.md should exist in output")
	}
	// Completion marker should be removed.
	if _, err := os.Stat(CompletionMarkerPath(outputDir)); !os.IsNotExist(err) {
		t.Error("completion marker should be removed after promotion")
	}
	// Journal should be cleaned up.
	if _, err := os.Stat(JournalPath(outputDir)); !os.IsNotExist(err) {
		t.Error("journal should be removed")
	}
}

func TestReconcileStateJournalWithTempNoMarker(t *testing.T) {
	base := t.TempDir()
	outputDir := filepath.Join(base, "output")
	tempDir := filepath.Join(base, "temp-staged")

	// Simulate crash: temp exists but no completion marker (incomplete staging).
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "partial.md"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	journal := Journal{
		TempDir:   tempDir,
		OutputDir: outputDir,
		BackupDir: outputDir + ".bak",
		Phase:     phaseStaged,
	}
	if err := WriteJournal(outputDir, journal); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileState(outputDir, nil); err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Temp should be removed (incomplete).
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Error("incomplete temp directory should be removed")
	}
	// Output should not exist.
	if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
		t.Error("output should not exist")
	}
}

// ---------- Options.removeAll ----------

func TestOptionsRemoveAllDefault(t *testing.T) {
	fn := (*Options)(nil).removeAll()
	if fn == nil {
		t.Fatal("default removeAll should not be nil")
	}
}

func TestOptionsRemoveAllCustom(t *testing.T) {
	called := false
	opts := &Options{
		RemoveAll: func(s string) error {
			called = true
			return os.RemoveAll(s)
		},
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "removeme")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	fn := opts.removeAll()
	if err := fn(target); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("custom RemoveAll was not called")
	}
}
