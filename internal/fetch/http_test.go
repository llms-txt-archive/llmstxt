package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeLastModified(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"invalid", "not-a-date", ""},
		{"RFC1123", "Mon, 02 Jan 2006 15:04:05 GMT", "2006-01-02T15:04:05Z"},
		{"RFC850", "Monday, 02-Jan-06 15:04:05 GMT", "2006-01-02T15:04:05Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeLastModified(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeLastModified(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIfModifiedSinceHeader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"invalid", "not-a-date", ""},
		{"RFC3339", "2006-01-02T15:04:05Z", "Mon, 02 Jan 2006 15:04:05 GMT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IfModifiedSinceHeader(tt.input)
			if got != tt.want {
				t.Errorf("IfModifiedSinceHeader(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeETag(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"abc123"`, `"abc123"`},
		{`  "abc123"  `, `"abc123"`},
		{"", ""},
		{"W/\"abc\"", "W/\"abc\""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeETag(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeETag(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCoalesceValidator(t *testing.T) {
	tests := []struct {
		current  string
		previous string
		want     string
	}{
		{"current", "previous", "current"},
		{"", "previous", "previous"},
		{"", "", ""},
		{"current", "", "current"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q_%q", tt.current, tt.previous), func(t *testing.T) {
			got := CoalesceValidator(tt.current, tt.previous)
			if got != tt.want {
				t.Errorf("CoalesceValidator(%q, %q) = %q, want %q", tt.current, tt.previous, got, tt.want)
			}
		})
	}
}

func TestCleanupSpoolFile(t *testing.T) {
	// Empty path should not panic.
	CleanupSpoolFile("")

	// Create a temp file and verify it gets removed.
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.tmp")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	CleanupSpoolFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, got err: %v", err)
	}

	// Non-existent file should not panic.
	CleanupSpoolFile(filepath.Join(dir, "nonexistent"))
}

func TestBuildUserAgent(t *testing.T) {
	ua := buildUserAgent()
	want := fmt.Sprintf("llmstxt-sync/2.0 (%s %s)", runtime.GOOS, runtime.GOARCH)
	if ua != want {
		t.Errorf("buildUserAgent() = %q, want %q", ua, want)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantZero bool
	}{
		{"empty", "", true},
		{"valid_seconds", "30", false},
		{"zero_seconds", "0", true},
		{"negative_seconds", "-1", true},
		{"too_large", "301", true},
		{"non_numeric", "abc", true},
		{"boundary_300", "300", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.input)
			if tt.wantZero && got != 0 {
				t.Errorf("parseRetryAfter(%q) = %v, want 0", tt.input, got)
			}
			if !tt.wantZero && got == 0 {
				t.Errorf("parseRetryAfter(%q) = 0, want non-zero", tt.input)
			}
		})
	}
}

func TestTransientHTTPErrorMessage(t *testing.T) {
	err := &TransientHTTPError{StatusCode: 503, Status: "503 Service Unavailable"}
	got := err.Error()
	want := "transient HTTP 503 Service Unavailable"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
