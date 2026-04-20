package fetch

import (
	"errors"
	"testing"
)

func TestEnsureMarkdownResponseHTMLContentTypeWithHTMLBody(t *testing.T) {
	sniff := []byte("<!DOCTYPE html><html><body>not found</body></html>")
	err := EnsureMarkdownResponse("200 OK", "text/html", nil, sniff, "")
	if err == nil {
		t.Fatal("expected error for HTML content-type with HTML body")
	}
	var unexpected *UnexpectedContentError
	if !errors.As(err, &unexpected) {
		t.Fatalf("expected UnexpectedContentError, got %T", err)
	}
}

func TestEnsureMarkdownResponseHTMLContentTypeWithMarkdownBody(t *testing.T) {
	sniff := []byte("# API Reference\n\nThis is the API reference.")
	err := EnsureMarkdownResponse("200 OK", "text/html", nil, sniff, "")
	if err != nil {
		t.Fatalf("unexpected error: servers may return text/html for markdown content: %v", err)
	}
}

func TestEnsureMarkdownResponseXHTMLWithHTMLBody(t *testing.T) {
	sniff := []byte("<!DOCTYPE html><html><body>page</body></html>")
	err := EnsureMarkdownResponse("200 OK", "application/xhtml+xml", nil, sniff, "")
	if err == nil {
		t.Fatal("expected error for XHTML content-type with HTML body")
	}
}

func TestEnsureMarkdownResponseHTMLSniff(t *testing.T) {
	sniff := []byte("<!DOCTYPE html><html><body>hello</body></html>")
	err := EnsureMarkdownResponse("200 OK", "text/plain", nil, sniff, "")
	if err == nil {
		t.Fatal("expected error for HTML-sniffed content")
	}
}

func TestEnsureMarkdownResponsePass(t *testing.T) {
	err := EnsureMarkdownResponse("200 OK", "text/plain", nil, []byte("# Hello"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLooksLikeHTMLDocumentBOM(t *testing.T) {
	body := []byte("\ufeff<!DOCTYPE html>")
	if !LooksLikeHTMLDocument(body) {
		t.Fatal("expected true for BOM + doctype")
	}
}

func TestLooksLikeHTMLDocumentXMLDecl(t *testing.T) {
	body := []byte("<?xml version=\"1.0\"?><!DOCTYPE html>")
	if !LooksLikeHTMLDocument(body) {
		t.Fatal("expected true for XML decl + doctype")
	}
}

func TestLooksLikeHTMLDocumentComment(t *testing.T) {
	body := []byte("<!-- comment --><html>")
	if !LooksLikeHTMLDocument(body) {
		t.Fatal("expected true for comment + html tag")
	}
}

func TestLooksLikeHTMLDocumentMarkdown(t *testing.T) {
	body := []byte("# Hello World\n\nThis is markdown.")
	if LooksLikeHTMLDocument(body) {
		t.Fatal("expected false for markdown")
	}
}

func TestPrefixCaptureWriter(t *testing.T) {
	w := &PrefixCaptureWriter{Limit: 5}
	_, _ = w.Write([]byte("hel"))
	_, _ = w.Write([]byte("lo world"))
	if string(w.Buf) != "hello" {
		t.Fatalf("PrefixCaptureWriter.Buf = %q, want %q", w.Buf, "hello")
	}
}
