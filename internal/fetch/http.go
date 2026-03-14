package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// HTMLSniffBytes is the number of leading bytes captured for content-type
// sniffing, matching the net/http sniffing buffer size.
const HTMLSniffBytes = 4096

// HTTPResponse captures the relevant fields from an HTTP response after downloading the body.
type HTTPResponse struct {
	Status         string
	ContentType    string
	Headers        map[string][]string
	LastModifiedAt string
	ETag           string
	NotModified    bool
	BodyPath       string
	Sniff          []byte
	SHA256         string
	Bytes          int64
}

// FetchURL performs an HTTP GET, writes the response body to a spool file, and returns the response metadata.
func FetchURL(ctx context.Context, client *http.Client, rawURL string, spoolDir string, validators Validators) (response HTTPResponse, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return HTTPResponse{}, err
	}

	req.Header.Set("User-Agent", buildUserAgent())
	req.Header.Set("Accept", "text/plain, text/markdown, text/*;q=0.9, */*;q=0.1")
	if ifNoneMatch := NormalizeETag(validators.ETag); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifModifiedSince := IfModifiedSinceHeader(validators.LastModifiedAt); ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := client.Do(req)
	if err != nil {
		return HTTPResponse{}, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()

	response = HTTPResponse{
		Status:         resp.Status,
		ContentType:    strings.TrimSpace(resp.Header.Get("Content-Type")),
		Headers:        copyHeader(resp.Header),
		LastModifiedAt: NormalizeLastModified(resp.Header.Get("Last-Modified")),
		ETag:           NormalizeETag(resp.Header.Get("ETag")),
		NotModified:    resp.StatusCode == http.StatusNotModified,
	}

	if resp.StatusCode == http.StatusNotModified {
		response.LastModifiedAt = CoalesceValidator(response.LastModifiedAt, validators.LastModifiedAt)
		response.ETag = CoalesceValidator(response.ETag, validators.ETag)
		return response, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HTTPResponse{}, fmt.Errorf("unexpected HTTP %s", resp.Status)
	}

	spoolFile, err := os.CreateTemp(spoolDir, "fetch-*")
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("create spool file: %w", err)
	}

	cleanupOnError := true
	defer func() {
		_ = spoolFile.Close()
		if cleanupOnError {
			_ = os.Remove(spoolFile.Name())
		}
	}()

	hasher := sha256.New()
	capture := &PrefixCaptureWriter{Limit: HTMLSniffBytes}

	written, err := io.Copy(io.MultiWriter(spoolFile, hasher, capture), resp.Body)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("stream response body: %w", err)
	}

	response.BodyPath = spoolFile.Name()
	response.Sniff = append([]byte(nil), capture.Buf...)
	response.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	response.Bytes = written

	cleanupOnError = false
	return response, nil
}

func copyHeader(header http.Header) map[string][]string {
	if len(header) == 0 {
		return nil
	}

	cloned := make(map[string][]string, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func buildUserAgent() string {
	return fmt.Sprintf("claudecodedocs-sync/2.0 (%s %s)", runtime.GOOS, runtime.GOARCH)
}

// IfModifiedSinceHeader converts an RFC 3339 timestamp into an HTTP-date suitable for If-Modified-Since.
func IfModifiedSinceHeader(lastModifiedAt string) string {
	if lastModifiedAt == "" {
		return ""
	}

	parsed, err := time.Parse(time.RFC3339, lastModifiedAt)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(http.TimeFormat)
}

// NormalizeLastModified parses an HTTP Last-Modified header and returns it in RFC 3339 format.
func NormalizeLastModified(value string) string {
	if value == "" {
		return ""
	}

	parsed, err := http.ParseTime(value)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(time.RFC3339)
}

// NormalizeETag trims whitespace from an ETag header value.
func NormalizeETag(value string) string {
	return strings.TrimSpace(value)
}

// CoalesceValidator returns current if non-empty, otherwise falls back to previous.
func CoalesceValidator(current string, previous string) string {
	if current != "" {
		return current
	}
	return previous
}
