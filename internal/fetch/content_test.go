package fetch

import (
	"errors"
	"testing"
)

func TestEnsureMarkdownResponseHTML(t *testing.T) {
	err := EnsureMarkdownResponse("200 OK", "text/html", nil, nil, "")
	if err == nil {
		t.Fatal("expected error for HTML content-type")
	}
	var unexpected *UnexpectedContentError
	if !errors.As(err, &unexpected) {
		t.Fatalf("expected UnexpectedContentError, got %T", err)
	}
}

func TestEnsureMarkdownResponseXHTML(t *testing.T) {
	err := EnsureMarkdownResponse("200 OK", "application/xhtml+xml", nil, nil, "")
	if err == nil {
		t.Fatal("expected error for XHTML content-type")
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
