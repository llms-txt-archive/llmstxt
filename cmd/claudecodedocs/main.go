package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultWorkers   = 8
	defaultLayout    = "nested"
	layoutRoot       = "root"
	layoutNested     = "nested"
	nonMarkdownReason = "non_markdown"
)

var markdownLinkPattern = regexp.MustCompile(`\((https?://[^)\s]+)\)`)
var plainURLLinePattern = regexp.MustCompile(`^(?:[-*+]\s+|\d+\.\s+)?(https?://\S+)$`)

type config struct {
	sourceURL            string
	outputDir            string
	layout               string
	previousManifestPath string
	manifestOut          string
	timeout              time.Duration
	concurrency          int
}

type fetchResult struct {
	URL            string
	RelativePath   string
	Body           []byte
	SHA256         string
	Bytes          int
	LastModifiedAt string
	ETag           string
}

type fetchFailure struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

type manifest struct {
	SourceURL            string          `json:"source_url"`
	SourcePath           string          `json:"source_path"`
	SourceSHA256         string          `json:"source_sha256"`
	SourceLastModifiedAt string          `json:"source_last_modified_at,omitempty"`
	SourceETag           string          `json:"source_etag,omitempty"`
	DocumentCount        int             `json:"document_count"`
	SkippedCount         int             `json:"skipped_count,omitempty"`
	Documents            []manifestEntry `json:"documents"`
	Skipped              []skippedEntry  `json:"skipped,omitempty"`
	Failures             []fetchFailure  `json:"failures,omitempty"`
}

type manifestEntry struct {
	URL            string `json:"url"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	Bytes          int    `json:"bytes"`
	LastModifiedAt string `json:"last_modified_at,omitempty"`
	ETag           string `json:"etag,omitempty"`
}

type skippedEntry struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

type partialSyncError struct {
	failures []fetchFailure
}

func (e *partialSyncError) Error() string {
	if len(e.failures) == 0 {
		return "partial sync"
	}

	lines := make([]string, 0, len(e.failures)+1)
	lines = append(lines, fmt.Sprintf("%d fetches failed", len(e.failures)))
	for _, failure := range e.failures {
		lines = append(lines, fmt.Sprintf("- %s: %s", failure.URL, failure.Error))
	}

	return strings.Join(lines, "\n")
}

func main() {
	log.SetFlags(0)

	cfg := parseFlags()
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.sourceURL, "source", "", "URL of the llms.txt index")
	flag.StringVar(&cfg.outputDir, "out", "", "directory where the generated snapshot is written")
	flag.StringVar(&cfg.layout, "layout", defaultLayout, "output layout: root or nested")
	flag.StringVar(&cfg.previousManifestPath, "previous-manifest", "", "path to a previously released manifest.json")
	flag.StringVar(&cfg.manifestOut, "manifest-out", "", "path where the fresh manifest.json is written")
	flag.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "HTTP timeout per request")
	flag.IntVar(&cfg.concurrency, "concurrency", defaultWorkers, "maximum number of concurrent fetches")
	flag.Parse()

	if cfg.sourceURL == "" {
		log.Fatal("missing required -source")
	}
	if cfg.outputDir == "" {
		log.Fatal("missing required -out")
	}
	if cfg.manifestOut == "" {
		log.Fatal("missing required -manifest-out")
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}
	if cfg.layout != layoutRoot && cfg.layout != layoutNested {
		log.Fatalf("invalid -layout %q (expected %q or %q)", cfg.layout, layoutRoot, layoutNested)
	}

	return cfg
}

func run(cfg config) error {
	client := &http.Client{Timeout: cfg.timeout}

	previousManifest, err := loadManifest(cfg.previousManifestPath)
	if err != nil {
		return err
	}

	previousDocuments := previousDocumentsByURL(previousManifest)

	sourcePath := sourcePathForLayout(cfg.layout)
	sourceResult, err := fetchDocument(
		client,
		cfg.sourceURL,
		sourcePath,
		sourceValidators(previousManifest),
	)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", cfg.sourceURL, err)
	}

	links, err := extractLinks(sourceResult.Body)
	if err != nil {
		return err
	}

	docURLs, skipped, err := partitionDocumentURLs(links)
	if err != nil {
		return err
	}

	documents, failures := fetchDocuments(client, cfg.layout, docURLs, cfg.concurrency, previousDocuments)

	manifestData := buildManifest(sourceResult, documents, skipped, failures)
	if err := writeManifest(cfg.manifestOut, manifestData); err != nil {
		return err
	}

	if len(failures) > 0 {
		return &partialSyncError{failures: failures}
	}

	if err := stageOutput(cfg.outputDir, sourceResult, documents); err != nil {
		return err
	}

	fmt.Printf(
		"Fetched %d markdown documents into %s (%d skipped non-markdown URLs)\n",
		len(documents),
		cfg.outputDir,
		len(skipped),
	)
	return nil
}

func extractLinks(body []byte) ([]string, error) {
	if links := extractMarkdownLinks(body); len(links) > 0 {
		return links, nil
	}

	if links := extractPlainURLLines(body); len(links) > 0 {
		return links, nil
	}

	return nil, errors.New("no document URLs found in llms.txt")
}

func extractMarkdownLinks(body []byte) []string {
	matches := markdownLinkPattern.FindAllSubmatch(body, -1)
	links := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))

	for _, match := range matches {
		link := strings.TrimRight(string(match[1]), ".,;:)")
		if _, exists := seen[link]; exists {
			continue
		}
		seen[link] = struct{}{}
		links = append(links, link)
	}

	sort.Strings(links)
	return links
}

func extractPlainURLLines(body []byte) []string {
	lines := strings.Split(string(body), "\n")
	links := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))

	for _, line := range lines {
		match := plainURLLinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 2 {
			continue
		}

		link := strings.TrimRight(match[1], ".,;:)")
		if _, exists := seen[link]; exists {
			continue
		}
		seen[link] = struct{}{}
		links = append(links, link)
	}

	sort.Strings(links)
	return links
}

func partitionDocumentURLs(links []string) ([]string, []skippedEntry, error) {
	docURLs := make([]string, 0, len(links))
	skipped := make([]skippedEntry, 0)

	for _, link := range links {
		isMarkdown, err := isMarkdownURL(link)
		if err != nil {
			return nil, nil, fmt.Errorf("classify %s: %w", link, err)
		}
		if !isMarkdown {
			skipped = append(skipped, skippedEntry{URL: link, Reason: nonMarkdownReason})
			continue
		}
		docURLs = append(docURLs, link)
	}

	return docURLs, skipped, nil
}

func isMarkdownURL(rawURL string) (bool, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false, err
	}

	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false, fmt.Errorf("missing scheme or host")
	}

	return strings.EqualFold(path.Ext(parsedURL.Path), ".md"), nil
}

func loadManifest(manifestPath string) (*manifest, error) {
	if manifestPath == "" {
		return nil, nil
	}

	body, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifestData manifest
	if err := json.Unmarshal(body, &manifestData); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	return &manifestData, nil
}

func sourceValidators(manifestData *manifest) manifestEntry {
	if manifestData == nil {
		return manifestEntry{}
	}

	return manifestEntry{
		LastModifiedAt: manifestData.SourceLastModifiedAt,
		ETag:           manifestData.SourceETag,
	}
}

func previousDocumentsByURL(manifestData *manifest) map[string]manifestEntry {
	if manifestData == nil {
		return nil
	}

	documents := make(map[string]manifestEntry, len(manifestData.Documents))
	for _, document := range manifestData.Documents {
		documents[document.URL] = document
	}

	return documents
}

func fetchDocuments(client *http.Client, layout string, docURLs []string, concurrency int, previousDocuments map[string]manifestEntry) ([]fetchResult, []fetchFailure) {
	type job struct {
		index int
		url   string
	}

	results := make([]fetchResult, len(docURLs))
	succeeded := make([]bool, len(docURLs))
	failures := make([]fetchFailure, 0)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	jobs := make(chan job)
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				relativePath, err := relativePathForURL(job.url, layout)
				if err != nil {
					mu.Lock()
					failures = append(failures, fetchFailure{URL: job.url, Error: fmt.Sprintf("map URL: %v", err)})
					mu.Unlock()
					continue
				}

				previous := previousDocuments[job.url]
				result, err := fetchDocument(client, job.url, relativePath, previous)
				if err != nil {
					mu.Lock()
					failures = append(failures, fetchFailure{URL: job.url, Error: err.Error()})
					mu.Unlock()
					continue
				}

				mu.Lock()
				results[job.index] = result
				succeeded[job.index] = true
				mu.Unlock()
			}
		}()
	}

	for index, docURL := range docURLs {
		jobs <- job{index: index, url: docURL}
	}
	close(jobs)
	wg.Wait()

	finalResults := make([]fetchResult, 0, len(docURLs))
	for index, ok := range succeeded {
		if ok {
			finalResults = append(finalResults, results[index])
		}
	}

	sort.Slice(failures, func(i, j int) bool {
		return failures[i].URL < failures[j].URL
	})

	return finalResults, failures
}

func fetchDocument(client *http.Client, rawURL string, relativePath string, previous manifestEntry) (fetchResult, error) {
	body, lastModifiedAt, etag, notModified, err := fetchURL(client, rawURL, previous.LastModifiedAt, previous.ETag)
	if err != nil {
		return fetchResult{}, err
	}

	if notModified {
		body, err = readExistingFile(relativePath)
		if err != nil {
			return fetchResult{}, err
		}
	}

	return fetchResult{
		URL:            rawURL,
		RelativePath:   relativePath,
		Body:           body,
		SHA256:         hashBytes(body),
		Bytes:          len(body),
		LastModifiedAt: lastModifiedAt,
		ETag:           etag,
	}, nil
}

func fetchURL(client *http.Client, rawURL string, previousLastModifiedAt string, previousETag string) ([]byte, string, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", false, err
	}

	req.Header.Set("User-Agent", buildUserAgent())
	req.Header.Set("Accept", "text/plain, text/markdown, text/*;q=0.9, */*;q=0.1")
	if ifNoneMatch := normalizeETag(previousETag); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifModifiedSince := ifModifiedSinceHeader(previousLastModifiedAt); ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil,
			coalesceValidator(normalizeLastModified(resp.Header.Get("Last-Modified")), previousLastModifiedAt),
			coalesceValidator(normalizeETag(resp.Header.Get("ETag")), previousETag),
			true,
			nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", false, fmt.Errorf("unexpected HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", false, err
	}

	return body,
		normalizeLastModified(resp.Header.Get("Last-Modified")),
		normalizeETag(resp.Header.Get("ETag")),
		false,
		nil
}

func buildUserAgent() string {
	return fmt.Sprintf("claudecodedocs-sync/2.0 (%s %s)", runtime.GOOS, runtime.GOARCH)
}

func ifModifiedSinceHeader(lastModifiedAt string) string {
	if lastModifiedAt == "" {
		return ""
	}

	parsed, err := time.Parse(time.RFC3339, lastModifiedAt)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(http.TimeFormat)
}

func normalizeLastModified(value string) string {
	if value == "" {
		return ""
	}

	parsed, err := http.ParseTime(value)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(time.RFC3339)
}

func normalizeETag(value string) string {
	return strings.TrimSpace(value)
}

func coalesceValidator(current string, previous string) string {
	if current != "" {
		return current
	}
	return previous
}

func sourcePathForLayout(layout string) string {
	if layout == layoutRoot {
		return "llms.txt"
	}
	return filepath.ToSlash(filepath.Join("source", "llms.txt"))
}

func relativePathForURL(rawURL string, layout string) (string, error) {
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

	if layout == layoutRoot {
		return filepath.FromSlash(trimmed), nil
	}

	return filepath.Join("pages", host, filepath.FromSlash(trimmed)), nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func readExistingFile(relativePath string) ([]byte, error) {
	body, err := os.ReadFile(filepath.Clean(relativePath))
	if err != nil {
		return nil, fmt.Errorf("read cached file %s: %w", relativePath, err)
	}

	return body, nil
}

func buildManifest(source fetchResult, documents []fetchResult, skipped []skippedEntry, failures []fetchFailure) manifest {
	manifestData := manifest{
		SourceURL:            source.URL,
		SourcePath:           filepath.ToSlash(source.RelativePath),
		SourceSHA256:         source.SHA256,
		SourceLastModifiedAt: source.LastModifiedAt,
		SourceETag:           source.ETag,
		DocumentCount:        len(documents),
		SkippedCount:         len(skipped),
		Documents:            make([]manifestEntry, 0, len(documents)),
	}

	if len(skipped) > 0 {
		manifestData.Skipped = append(manifestData.Skipped, skipped...)
	}
	if len(failures) > 0 {
		manifestData.Failures = append(manifestData.Failures, failures...)
	}

	for _, document := range documents {
		manifestData.Documents = append(manifestData.Documents, manifestEntry{
			URL:            document.URL,
			Path:           filepath.ToSlash(document.RelativePath),
			SHA256:         document.SHA256,
			Bytes:          document.Bytes,
			LastModifiedAt: document.LastModifiedAt,
			ETag:           document.ETag,
		})
	}

	return manifestData
}

func writeManifest(manifestPath string, manifestData manifest) error {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}

	manifestBytes, err := json.MarshalIndent(manifestData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}

func stageOutput(outputDir string, source fetchResult, documents []fetchResult) error {
	parentDir := filepath.Dir(outputDir)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tempDir, err := os.MkdirTemp(parentDir, ".claudecodedocs-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}

	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.RemoveAll(tempDir)
		}
	}()

	if err := writeResult(tempDir, source); err != nil {
		return err
	}

	for _, document := range documents {
		if err := writeResult(tempDir, document); err != nil {
			return err
		}
	}

	if err := replaceDir(tempDir, outputDir); err != nil {
		return err
	}

	cleanupTemp = false
	return nil
}

func writeResult(root string, result fetchResult) error {
	targetPath := filepath.Join(root, filepath.FromSlash(result.RelativePath))
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", targetPath, err)
	}

	if err := os.WriteFile(targetPath, result.Body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	return nil
}

func replaceDir(tempDir string, outputDir string) error {
	backupDir := outputDir + ".bak"
	_ = os.RemoveAll(backupDir)

	if _, err := os.Stat(outputDir); err == nil {
		if err := os.Rename(outputDir, backupDir); err != nil {
			return fmt.Errorf("backup existing output: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat output directory: %w", err)
	}

	if err := os.Rename(tempDir, outputDir); err != nil {
		if _, restoreErr := os.Stat(backupDir); restoreErr == nil {
			_ = os.Rename(backupDir, outputDir)
		}
		return fmt.Errorf("activate new output: %w", err)
	}

	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove backup directory: %w", err)
	}

	return nil
}
