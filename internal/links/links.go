package links

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"claudecodedocs/internal/manifest"
)

const (
	LayoutRoot        = "root"
	LayoutNested      = "nested"
	NonMarkdownReason = "non_markdown"
)

var markdownLinkPattern = regexp.MustCompile(`\((https?://[^)\s]+)\)`)
var plainURLLinePattern = regexp.MustCompile(`^(?:[-*+]\s+|\d+\.\s+)?(https?://\S+)$`)

func ExtractLinks(body []byte) ([]string, error) {
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
		return nil, fmt.Errorf("no document URLs found in llms.txt")
	}

	return links, nil
}

func PartitionDocumentURLs(links []string) ([]string, []manifest.SkippedEntry, error) {
	docURLs := make([]string, 0, len(links))
	skipped := make([]manifest.SkippedEntry, 0)

	for _, link := range links {
		isMarkdown, err := IsMarkdownURL(link)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect link %q: %w", link, err)
		}

		if isMarkdown {
			docURLs = append(docURLs, link)
			continue
		}

		skipped = append(skipped, manifest.SkippedEntry{URL: link, Reason: NonMarkdownReason})
	}

	return docURLs, skipped, nil
}

func IsMarkdownURL(rawURL string) (bool, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false, err
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false, fmt.Errorf("missing scheme or host")
	}

	return strings.EqualFold(path.Ext(parsedURL.Path), ".md"), nil
}

func SourcePathForLayout(layout string) string {
	if layout == LayoutRoot {
		return "llms.txt"
	}
	return filepath.ToSlash(filepath.Join("source", "llms.txt"))
}

func RelativePathForURL(rawURL string, layout string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	host := parsedURL.Hostname()
	if host == "" {
		return "", fmt.Errorf("missing host in %q", rawURL)
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
