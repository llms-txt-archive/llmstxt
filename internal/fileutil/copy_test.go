package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	dst := filepath.Join(t.TempDir(), "dst.txt")
	os.WriteFile(src, []byte("hello"), 0o600)

	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile() error = %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Fatalf("CopyFile() wrote %q, want %q", got, "hello")
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("CopyFile() perm = %o, want 0644", info.Mode().Perm())
	}
}

func TestCopyFileSourceNotFound(t *testing.T) {
	err := CopyFile("/nonexistent", filepath.Join(t.TempDir(), "dst"))
	if err == nil {
		t.Fatal("CopyFile() expected error for missing source")
	}
}

func TestCopyFileCleansUpOnFailure(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	os.WriteFile(src, []byte("data"), 0o600)
	dst := filepath.Join(t.TempDir(), "nodir", "sub", "dst.txt")
	err := CopyFile(src, dst)
	if err == nil {
		t.Fatal("CopyFile() expected error")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatal("CopyFile() did not clean up target on failure")
	}
}
