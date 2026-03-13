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
	defaultSourceURL = "https://code.claude.com/docs/llms.txt"
	defaultOutputDir = "snapshot"
	defaultTimeout   = 30 * time.Second
	defaultWorkers   = 8
)

var markdownLinkPattern = regexp.MustCompile(`\((https?://[^)\s]+)\)`)

type config struct {
	sourceURL   string
	outputDir   string
	timeout     time.Duration
	concurrency int
}

type fetchResult struct {
	URL            string
	RelativePath   string
	Body           []byte
	SHA256         string
	Bytes          int
	LastModifiedAt string
}

type manifest struct {
	SourceURL            string          `json:"source_url"`
	SourcePath           string          `json:"source_path"`
	SourceSHA256         string          `json:"source_sha256"`
	SourceLastModifiedAt string          `json:"source_last_modified_at,omitempty"`
	DocumentCount        int             `json:"document_count"`
	Documents            []manifestEntry `json:"documents"`
}

type manifestEntry struct {
	URL            string `json:"url"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	Bytes          int    `json:"bytes"`
	LastModifiedAt string `json:"last_modified_at,omitempty"`
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

	flag.StringVar(&cfg.sourceURL, "source", defaultSourceURL, "URL of the llms.txt index")
	flag.StringVar(&cfg.outputDir, "out", defaultOutputDir, "directory where the fetched snapshot is written")
	flag.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "HTTP timeout per request")
	flag.IntVar(&cfg.concurrency, "concurrency", defaultWorkers, "maximum number of concurrent fetches")
	flag.Parse()

	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}

	return cfg
}

func run(cfg config) error {
	client := &http.Client{Timeout: cfg.timeout}
	previousManifest, err := loadManifest(cfg.outputDir)
	if err != nil {
		return err
	}

	previousDocuments := previousDocumentsByURL(previousManifest)

	sourcePath := filepath.ToSlash(filepath.Join("source", "llms.txt"))
	sourceLastModifiedAt := ""
	if previousManifest != nil {
		sourceLastModifiedAt = previousManifest.SourceLastModifiedAt
	}

	sourceResult, err := fetchDocument(client, cfg.outputDir, cfg.sourceURL, sourcePath, sourceLastModifiedAt)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", cfg.sourceURL, err)
	}

	sourceBody := sourceResult.Body
	docURLs, err := extractLinks(sourceBody)
	if err != nil {
		return err
	}

	documents, err := fetchDocuments(client, cfg.outputDir, docURLs, cfg.concurrency, previousDocuments)
	if err != nil {
		return err
	}

	manifestData := manifest{
		SourceURL:            cfg.sourceURL,
		SourcePath:           sourcePath,
		SourceSHA256:         sourceResult.SHA256,
		SourceLastModifiedAt: sourceResult.LastModifiedAt,
		DocumentCount:        len(documents),
		Documents:            make([]manifestEntry, 0, len(documents)),
	}

	for _, document := range documents {
		manifestData.Documents = append(manifestData.Documents, manifestEntry{
			URL:            document.URL,
			Path:           filepath.ToSlash(document.RelativePath),
			SHA256:         document.SHA256,
			Bytes:          document.Bytes,
			LastModifiedAt: document.LastModifiedAt,
		})
	}

	if err := stageOutput(cfg.outputDir, sourceResult, documents, manifestData); err != nil {
		return err
	}

	fmt.Printf("Fetched %d documents into %s\n", len(documents), cfg.outputDir)
	return nil
}

func extractLinks(body []byte) ([]string, error) {
	matches := markdownLinkPattern.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, errors.New("no document URLs found in llms.txt")
	}

	seen := make(map[string]struct{}, len(matches))
	links := make([]string, 0, len(matches))
	for _, match := range matches {
		link := string(match[1])
		if _, exists := seen[link]; exists {
			continue
		}
		seen[link] = struct{}{}
		links = append(links, link)
	}

	sort.Strings(links)
	return links, nil
}

func loadManifest(outputDir string) (*manifest, error) {
	manifestPath := filepath.Join(outputDir, "manifest.json")
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

func fetchDocuments(client *http.Client, outputDir string, docURLs []string, concurrency int, previousDocuments map[string]manifestEntry) ([]fetchResult, error) {
	type job struct {
		index int
		url   string
	}

	results := make([]fetchResult, len(docURLs))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)

	jobs := make(chan job)
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				relativePath, err := relativePathForURL(job.url)
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("map %s: %w", job.url, err))
					mu.Unlock()
					continue
				}

				previousLastModifiedAt := ""
				if previous, ok := previousDocuments[job.url]; ok {
					previousLastModifiedAt = previous.LastModifiedAt
				}

				result, err := fetchDocument(client, outputDir, job.url, relativePath, previousLastModifiedAt)
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("fetch %s: %w", job.url, err))
					mu.Unlock()
					continue
				}

				mu.Lock()
				results[job.index] = result
				mu.Unlock()
			}
		}()
	}

	for index, docURL := range docURLs {
		jobs <- job{index: index, url: docURL}
	}
	close(jobs)
	wg.Wait()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return results, nil
}

func fetchDocument(client *http.Client, outputDir string, rawURL string, relativePath string, previousLastModifiedAt string) (fetchResult, error) {
	body, lastModifiedAt, notModified, err := fetchURL(client, rawURL, previousLastModifiedAt)
	if err != nil {
		return fetchResult{}, err
	}

	if notModified {
		body, err = readExistingFile(outputDir, relativePath)
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
	}, nil
}

func fetchURL(client *http.Client, rawURL string, previousLastModifiedAt string) ([]byte, string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", false, err
	}

	req.Header.Set("User-Agent", buildUserAgent())
	req.Header.Set("Accept", "text/plain, text/markdown, text/*;q=0.9, */*;q=0.1")
	if ifModifiedSince := ifModifiedSinceHeader(previousLastModifiedAt); ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, previousLastModifiedAt, true, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", false, fmt.Errorf("unexpected HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", false, err
	}

	return body, normalizeLastModified(resp.Header.Get("Last-Modified")), false, nil
}

func buildUserAgent() string {
	return fmt.Sprintf("claudecodedocs-sync/1.0 (%s %s)", runtime.GOOS, runtime.GOARCH)
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

func relativePathForURL(rawURL string) (string, error) {
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

func readExistingFile(outputDir string, relativePath string) ([]byte, error) {
	targetPath := filepath.Join(outputDir, filepath.FromSlash(relativePath))
	body, err := os.ReadFile(targetPath)
	if err != nil {
		return nil, fmt.Errorf("read cached file %s: %w", targetPath, err)
	}

	return body, nil
}

func stageOutput(outputDir string, source fetchResult, documents []fetchResult, manifestData manifest) error {
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

	manifestPath := filepath.Join(tempDir, "manifest.json")
	manifestBytes, err := json.MarshalIndent(manifestData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
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
