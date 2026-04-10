package fileutil

import (
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
