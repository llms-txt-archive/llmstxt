package fetch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeJoinNormal(t *testing.T) {
	root := t.TempDir()
	got, err := SafeJoin(root, "docs/guide.md")
	if err != nil {
		t.Fatalf("SafeJoin() error = %v", err)
	}
	want := filepath.Join(root, "docs", "guide.md")
	if got != want {
		t.Fatalf("SafeJoin() = %q, want %q", got, want)
	}
}

func TestSafeJoinAbsoluteRejected(t *testing.T) {
	_, err := SafeJoin(t.TempDir(), "/etc/passwd")
	if err == nil {
		t.Fatal("SafeJoin() expected error for absolute path")
	}
}

func TestSafeJoinTraversalRejected(t *testing.T) {
	_, err := SafeJoin(t.TempDir(), "../../../etc/passwd")
	if err == nil {
		t.Fatal("SafeJoin() expected error for traversal")
	}
}

func TestSafeJoinEmptyRoot(t *testing.T) {
	_, err := SafeJoin("", "file.txt")
	if err == nil {
		t.Fatal("SafeJoin() expected error for empty root")
	}
}

func TestHashBytes(t *testing.T) {
	hash := HashBytes([]byte("hello"))
	// SHA-256 of "hello"
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if hash != want {
		t.Fatalf("HashBytes(\"hello\") = %q, want %q", hash, want)
	}
}

func TestCleanupSpoolFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "spool.tmp")
	os.WriteFile(f, []byte("data"), 0o600)
	CleanupSpoolFile(f)
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("CleanupSpoolFile() did not remove file")
	}
}

func TestCleanupSpoolFileEmpty(t *testing.T) { //nolint:revive // required by testing framework
	// Should not panic
	CleanupSpoolFile("")
}
