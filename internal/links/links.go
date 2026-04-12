// Package links extracts, classifies, and maps URLs found in an llms.txt document.
package links

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/llms-txt-archive/llmstxt/internal/manifest"
)

const (
	// LayoutRoot places fetched documents at the top level of the output directory.
	LayoutRoot = "root"
	// LayoutNested organizes fetched documents into subdirectories by host.
	LayoutNested = "nested"
	// NonMarkdownReason is the skip reason recorded for URLs that do not end in .md.
	NonMarkdownReason = "non_markdown"
)

// ErrNoDocumentURLs is returned when an llms.txt body contains no extractable URLs.
var ErrNoDocumentURLs = errors.New("no document URLs found in llms.txt")

var markdownLinkPattern = regexp.MustCompile(`\((https?://[^)\s]+)\)`)
var plainURLLinePattern = regexp.MustCompile(`^(?:[-*+]\s+|\d+\.\s+)?(https?://\S+)$`)

// Extract parses an llms.txt body and returns all unique HTTP(S) URLs found in it.
func Extract(body []byte) ([]string, error) {
	matches := markdownLinkPattern.FindAllSubmatch(body, -1)
	set := make(map[string]struct{})

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		set[string(match[1])] = struct{}{}
	}

	if len(set) == 0 {
		lines := strings.Split(string(body), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			match := plainURLLinePattern.FindStringSubmatch(line)
			if len(match) < 2 {
				continue
			}

			rawURL := strings.TrimRight(match[1], ".,;:")
			set[rawURL] = struct{}{}
		}
	}

	links := make([]string, 0, len(set))
	for link := range set {
		links = append(links, link)
	}
	sort.Strings(links)
	if len(links) == 0 {
		return nil, ErrNoDocumentURLs
	}

	return links, nil
}

// IsIndex reports whether rawURL points to an llms.txt index file (not llms-full.txt).
func IsIndex(rawURL string) bool {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return path.Base(parsedURL.Path) == "llms.txt"
}

// Partition splits URLs into markdown document URLs, nested llms.txt index URLs,
// and skipped non-markdown entries.
func Partition(links []string) (docURLs, indexURLs []string, skipped []manifest.SkippedEntry, err error) {
	docURLs = make([]string, 0, len(links))

	for _, link := range links {
		if IsIndex(link) {
			indexURLs = append(indexURLs, link)
			continue
		}

		isMarkdown, parseErr := IsMarkdown(link)
		if parseErr != nil {
			return nil, nil, nil, fmt.Errorf("inspect link %q: %w", link, parseErr)
		}

		if isMarkdown {
			docURLs = append(docURLs, link)
			continue
		}

		skipped = append(skipped, manifest.SkippedEntry{URL: link, Reason: NonMarkdownReason})
	}

	return docURLs, indexURLs, skipped, nil
}

// IsMarkdown reports whether rawURL has a .md file extension.
func IsMarkdown(rawURL string) (bool, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false, err
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false, fmt.Errorf("missing scheme or host")
	}

	return strings.EqualFold(path.Ext(parsedURL.Path), ".md"), nil
}

// SourcePath returns the relative file path for the llms.txt source document under the given layout.
func SourcePath(layout string) string {
	if layout == LayoutRoot {
		return "llms.txt"
	}
	return filepath.ToSlash(filepath.Join("source", "llms.txt"))
}

// RelativePath maps a document URL to a filesystem-relative path under the given layout.
func RelativePath(rawURL string, layout string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	host := parsedURL.Hostname()
	if host == "" {
		return "", fmt.Errorf("missing host in %q", rawURL)
	}
	if strings.ContainsAny(host, `/\`) || strings.Contains(host, "..") {
		return "", fmt.Errorf("invalid host in %q", rawURL)
	}

	urlPath := parsedURL.EscapedPath()
	if urlPath == "" {
		urlPath = "/"
	}

	cleanPath := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	trimmed := strings.TrimPrefix(cleanPath, "/")
	if trimmed == "" || trimmed == "." {
		trimmed = "index"
	}

	if strings.HasSuffix(parsedURL.Path, "/") {
		trimmed = path.Join(trimmed, "index.html")
	} else if path.Ext(trimmed) == "" {
		trimmed += ".html"
	}

	if parsedURL.RawQuery != "" {
		extension := path.Ext(trimmed)
		base := strings.TrimSuffix(trimmed, extension)
		trimmed = fmt.Sprintf("%s__%s%s", base, shortHash(parsedURL.RawQuery), extension)
	}

	if layout == LayoutRoot {
		return filepath.FromSlash(trimmed), nil
	}

	return filepath.Join("pages", host, filepath.FromSlash(trimmed)), nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}
