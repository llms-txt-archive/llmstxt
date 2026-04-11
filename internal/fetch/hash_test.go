package fetch

import "testing"

func TestHashBytesKnownValue(t *testing.T) {
	got := HashBytes([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("HashBytes(\"hello\") = %q, want %q", got, want)
	}
}

func TestHashBytesEmpty(t *testing.T) {
	got := HashBytes([]byte{})
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Fatalf("HashBytes(nil) = %q, want %q", got, want)
	}
}
