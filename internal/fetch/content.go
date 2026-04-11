package fetch

import (
	"fmt"
	"strings"
)

// UnexpectedContentError indicates that a response contained HTML or other non-markdown content.
// Callers can detect this with errors.As to access response details for diagnostics.
type UnexpectedContentError struct {
	Message     string
	Status      string
	ContentType string
	Headers     map[string][]string
	Sniff       []byte
	BodyPath    string
}

func (e *UnexpectedContentError) Error() string {
	return e.Message
}

// PrefixCaptureWriter is an io.Writer that retains the first Limit bytes written to it.
type PrefixCaptureWriter struct {
	Limit int
	Buf   []byte
}

func (w *PrefixCaptureWriter) Write(p []byte) (int, error) {
	if remaining := w.Limit - len(w.Buf); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.Buf = append(w.Buf, p[:remaining]...)
	}
	return len(p), nil
}

// EnsureMarkdownResponse returns an UnexpectedContentError if the response appears to be HTML rather than markdown.
// It first checks contentType (e.g. "text/html"), then falls back to sniffing the leading bytes of the body.
// The sniff parameter should contain the first few KB of the response body for content detection.
// The bodyPath and other parameters are stored in the returned error for diagnostic reporting.
func EnsureMarkdownResponse(status string, contentType string, headers map[string][]string, sniff []byte, bodyPath string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return &UnexpectedContentError{
			Message:     fmt.Sprintf("expected markdown response but received %s", mediaType),
			Status:      status,
			ContentType: contentType,
			Headers:     headers,
			Sniff:       append([]byte(nil), sniff...),
			BodyPath:    bodyPath,
		}
	}

	if LooksLikeHTMLDocument(sniff) {
		return &UnexpectedContentError{
			Message:     "expected markdown response but received HTML document",
			Status:      status,
			ContentType: contentType,
			Headers:     headers,
			Sniff:       append([]byte(nil), sniff...),
			BodyPath:    bodyPath,
		}
	}

	return nil
}

// LooksLikeHTMLDocument reports whether the leading bytes of body look like an HTML document.
func LooksLikeHTMLDocument(body []byte) bool {
	trimmed := NormalizeHTMLSniff(body)
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<head") ||
		strings.HasPrefix(lower, "<body")
}

// NormalizeHTMLSniff strips BOM, XML declarations, and HTML comments from body for content sniffing.
func NormalizeHTMLSniff(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")

	for {
		trimmed = strings.TrimSpace(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "<?xml"):
			end := strings.Index(trimmed, "?>")
			if end == -1 {
				return trimmed
			}
			trimmed = trimmed[end+2:]
		case strings.HasPrefix(trimmed, "<!--"):
			end := strings.Index(trimmed, "-->")
			if end == -1 {
				return trimmed
			}
			trimmed = trimmed[end+3:]
		default:
			return trimmed
		}
	}
}
