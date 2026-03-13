package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultWorkers        = 8
	defaultLayout         = "nested"
	markdownFetchAttempts = 3
	layoutRoot            = "root"
	layoutNested          = "nested"
	nonMarkdownReason     = "non_markdown"
	htmlSniffBytes        = 4096
	progressLogEvery      = 25
)

var markdownLinkPattern = regexp.MustCompile(`\((https?://[^)\s]+)\)`)
var plainURLLinePattern = regexp.MustCompile(`^(?:[-*+]\s+|\d+\.\s+)?(https?://\S+)$`)
var blockedIPPfx = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}
var retrySleepWithJitter = sleepWithJitter

type config struct {
	sourceURL            string
	outputDir            string
	layout               string
	previousManifestPath string
	manifestOut          string
	diagnosticsDir       string
	allowedHostsCSV      string
	snapshotRoot         string
	timeout              time.Duration
	concurrency          int
}

type fetchResult struct {
	URL            string
	RelativePath   string
	LocalPath      string
	SHA256         string
	Bytes          int64
	LastModifiedAt string
	ETag           string
}

type fetchFailure struct {
	URL               string `json:"url"`
	Error             string `json:"error"`
	DiagnosticPath    string `json:"diagnostic_path,omitempty"`
	PreservedExisting bool   `json:"preserved_existing,omitempty"`
}

type manifest struct {
	SourceURL            string          `json:"source_url"`
	SourcePath           string          `json:"source_path"`
	SourceSHA256         string          `json:"source_sha256,omitempty"`
	SourceLastModifiedAt string          `json:"source_last_modified_at,omitempty"`
	SourceETag           string          `json:"source_etag,omitempty"`
	DocumentCount        int             `json:"document_count"`
	SkippedCount         int             `json:"skipped_count,omitempty"`
	Documents            []manifestEntry `json:"documents,omitempty"`
	Skipped              []skippedEntry  `json:"skipped,omitempty"`
	Failures             []fetchFailure  `json:"failures,omitempty"`
}

type manifestEntry struct {
	URL            string `json:"url"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	Bytes          int64  `json:"bytes"`
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

type unexpectedContentError struct {
	message     string
	status      string
	contentType string
	headers     map[string][]string
	sniff       []byte
	bodyPath    string
}

type fetchValidators struct {
	LastModifiedAt string
	ETag           string
}

type httpFetchResponse struct {
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

type urlPolicy struct {
	allowedHosts map[string]struct{}
}

type prefixCaptureWriter struct {
	limit int
	buf   []byte
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

func (e *unexpectedContentError) Error() string {
	return e.message
}

func (w *prefixCaptureWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - len(w.buf); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.buf = append(w.buf, p[:remaining]...)
	}
	return len(p), nil
}

func main() {
	log.SetFlags(0)

	cfg := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
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
	flag.StringVar(&cfg.diagnosticsDir, "diagnostics-dir", "", "directory where fetch diagnostics are written on failure")
	flag.StringVar(&cfg.allowedHostsCSV, "allowed-hosts", "", "comma-separated additional hosts allowed for document fetches")
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

	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve current working directory: %v", err)
	}
	cfg.snapshotRoot = wd

	return cfg
}

func run(ctx context.Context, cfg config) error {
	policy, err := newURLPolicy(cfg.sourceURL, cfg.allowedHostsCSV)
	if err != nil {
		return err
	}

	if err := policy.Validate(cfg.sourceURL); err != nil {
		return fmt.Errorf("validate source URL: %w", err)
	}

	client := newHTTPClient(cfg.timeout, policy)

	spoolDir, err := os.MkdirTemp("", ".claudecodedocs-fetch-*")
	if err != nil {
		return fmt.Errorf("create fetch spool directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(spoolDir)
	}()

	previousManifest, err := loadManifest(cfg.previousManifestPath)
	if err != nil {
		return err
	}

	previousDocuments := previousDocumentsByURL(previousManifest)
	sourcePath := sourcePathForLayout(cfg.layout)
	sourcePrevious := previousSourceEntry(previousManifest, sourcePath)

	sourceResult, err := fetchDocument(ctx, client, policy, spoolDir, cfg.snapshotRoot, cfg.sourceURL, sourcePath, sourcePrevious)
	if err != nil {
		failures := []fetchFailure{buildFetchFailure(cfg.diagnosticsDir, cfg.sourceURL, sourcePath, err)}
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, nil, nil, nil, failures))
		return fmt.Errorf("fetch %s: %w", cfg.sourceURL, err)
	}

	sourceBody, err := os.ReadFile(sourceResult.LocalPath)
	if err != nil {
		failures := []fetchFailure{{URL: cfg.sourceURL, Error: fmt.Sprintf("read fetched llms.txt: %v", err)}}
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, &sourceResult, nil, nil, failures))
		return fmt.Errorf("read fetched llms.txt: %w", err)
	}

	links, err := extractLinks(sourceBody)
	if err != nil {
		failures := []fetchFailure{{URL: cfg.sourceURL, Error: err.Error()}}
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, &sourceResult, nil, nil, failures))
		return err
	}

	docURLs, skipped, err := partitionDocumentURLs(links)
	if err != nil {
		failures := []fetchFailure{{URL: cfg.sourceURL, Error: err.Error()}}
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, &sourceResult, nil, skipped, failures))
		return err
	}

	documents, failures := fetchDocuments(
		ctx,
		client,
		policy,
		cfg.layout,
		cfg.diagnosticsDir,
		spoolDir,
		cfg.snapshotRoot,
		docURLs,
		cfg.concurrency,
		previousDocuments,
	)
	if err := ctx.Err(); err != nil {
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, &sourceResult, documents, skipped, failures))
		return err
	}

	manifestData := buildManifest(sourceResult, documents, skipped, failures)
	if err := stageOutput(cfg.outputDir, sourceResult, documents); err != nil {
		failureWithStage := append([]fetchFailure(nil), failures...)
		failureWithStage = append(failureWithStage, fetchFailure{
			URL:   cfg.sourceURL,
			Error: fmt.Sprintf("stage output: %v", err),
		})
		writeDiagnosticManifest(cfg.manifestOut, buildDiagnosticManifest(cfg.sourceURL, sourcePath, &sourceResult, documents, skipped, failureWithStage))
		return err
	}

	if err := writeManifest(cfg.manifestOut, manifestData); err != nil {
		return err
	}

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "%s\n", (&partialSyncError{failures: failures}).Error())
	}

	fmt.Printf(
		"Fetched %d markdown documents into %s (%d skipped non-markdown URLs, %d fetch failures)\n",
		len(documents),
		cfg.outputDir,
		len(skipped),
		len(failures),
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

	// #nosec G304 -- manifestPath is a local CLI input to a release asset on disk.
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

func previousSourceEntry(manifestData *manifest, fallbackPath string) manifestEntry {
	if manifestData == nil {
		return manifestEntry{Path: fallbackPath}
	}

	sourcePath := manifestData.SourcePath
	if sourcePath == "" {
		sourcePath = fallbackPath
	}

	return manifestEntry{
		URL:            manifestData.SourceURL,
		Path:           sourcePath,
		SHA256:         manifestData.SourceSHA256,
		LastModifiedAt: manifestData.SourceLastModifiedAt,
		ETag:           manifestData.SourceETag,
	}
}

func newURLPolicy(sourceURL string, allowedHostsCSV string) (*urlPolicy, error) {
	parsedSource, err := url.Parse(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("parse source URL: %w", err)
	}

	sourceHost, err := normalizeHost(parsedSource.Hostname())
	if err != nil {
		return nil, fmt.Errorf("source host: %w", err)
	}

	allowedHosts := map[string]struct{}{sourceHost: {}}
	for _, field := range strings.Split(allowedHostsCSV, ",") {
		if strings.TrimSpace(field) == "" {
			continue
		}
		host, err := normalizeHost(field)
		if err != nil {
			return nil, fmt.Errorf("allowed host %q: %w", field, err)
		}
		allowedHosts[host] = struct{}{}
	}

	return &urlPolicy{allowedHosts: allowedHosts}, nil
}

func normalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	if host == "" {
		return "", errors.New("missing host")
	}
	if strings.Contains(host, "://") {
		return "", errors.New("expected hostname, not URL")
	}
	if ip := net.ParseIP(host); ip != nil {
		return "", errors.New("IP literal hosts are not allowed")
	}
	if strings.Contains(host, "/") {
		return "", errors.New("invalid host")
	}
	return host, nil
}

func (p *urlPolicy) Validate(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	return p.ValidateURL(parsedURL)
}

func (p *urlPolicy) ValidateURL(parsedURL *url.URL) error {
	if parsedURL == nil {
		return errors.New("missing URL")
	}
	if !strings.EqualFold(parsedURL.Scheme, "https") {
		return fmt.Errorf("scheme %q is not allowed", parsedURL.Scheme)
	}

	host, err := normalizeHost(parsedURL.Hostname())
	if err != nil {
		return err
	}

	if _, ok := p.allowedHosts[host]; !ok {
		return fmt.Errorf("host %q is not allowed", host)
	}
	return nil
}

func (p *urlPolicy) ValidateResolvedHost(ctx context.Context, host string) ([]string, error) {
	normalizedHost, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}
	if _, ok := p.allowedHosts[normalizedHost]; !ok {
		return nil, fmt.Errorf("host %q is not allowed", normalizedHost)
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, normalizedHost)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses returned", normalizedHost)
	}

	dialAddrs := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if err := validateResolvedIP(addr.IP); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
		}
		dialAddrs = append(dialAddrs, addr.IP.String())
	}

	return dialAddrs, nil
}

func validateResolvedIP(ip net.IP) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return errors.New("invalid resolved IP")
	}
	addr = addr.Unmap()

	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("resolved to blocked IP %s", addr)
	}
	for _, prefix := range blockedIPPfx {
		if prefix.Contains(addr) {
			return fmt.Errorf("resolved to blocked IP %s", addr)
		}
	}
	return nil
}

func newHTTPClient(timeout time.Duration, policy *urlPolicy) *http.Client {
	baseTransport, _ := http.DefaultTransport.(*http.Transport)
	transport := baseTransport.Clone()
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}

	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		if ip := net.ParseIP(host); ip != nil {
			if err := validateResolvedIP(ip); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, address)
		}

		dialAddrs, err := policy.ValidateResolvedHost(ctx, host)
		if err != nil {
			return nil, err
		}

		var lastErr error
		for _, dialAddr := range dialAddrs {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(dialAddr, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no dialable addresses for %s", host)
		}
		return nil, lastErr
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return policy.ValidateURL(req.URL)
		},
	}
}

func fetchDocuments(
	ctx context.Context,
	client *http.Client,
	policy *urlPolicy,
	layout string,
	diagnosticsDir string,
	spoolDir string,
	snapshotRoot string,
	docURLs []string,
	concurrency int,
	previousDocuments map[string]manifestEntry,
) ([]fetchResult, []fetchFailure) {
	type job struct {
		index int
		url   string
	}

	results := make([]fetchResult, len(docURLs))
	succeeded := make([]bool, len(docURLs))
	failures := make([]fetchFailure, 0)

	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		completed atomic.Int64
	)

	recordCompletion := func() {
		done := completed.Add(1)
		if done%progressLogEvery == 0 || done == int64(len(docURLs)) {
			log.Printf("Fetched %d/%d markdown documents", done, len(docURLs))
		}
	}

	recordFailure := func(jobURL string, failure fetchFailure) {
		log.Printf("Fetch failure for %s: %s", jobURL, failure.Error)
		mu.Lock()
		failures = append(failures, failure)
		mu.Unlock()
		recordCompletion()
	}

	jobs := make(chan job)
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}

					previous := previousDocuments[job.url]
					relativePath := filepath.FromSlash(previous.Path)
					if relativePath == "" {
						var err error
						relativePath, err = relativePathForURL(job.url, layout)
						if err != nil {
							recordFailure(job.url, fetchFailure{URL: job.url, Error: fmt.Sprintf("map URL: %v", err)})
							continue
						}
					}

					if err := policy.Validate(job.url); err != nil {
						failure := fetchFailure{URL: job.url, Error: err.Error()}
						preserved, preservedErr := preservePreviousDocument(snapshotRoot, job.url, relativePath, previous)
						if preservedErr == nil {
							failure.PreservedExisting = true
							mu.Lock()
							results[job.index] = preserved
							succeeded[job.index] = true
							failures = append(failures, failure)
							mu.Unlock()
							recordCompletion()
							continue
						}
						recordFailure(job.url, failure)
						continue
					}

					result, err := fetchDocument(ctx, client, policy, spoolDir, snapshotRoot, job.url, relativePath, previous)
					if err != nil {
						failure := buildFetchFailure(diagnosticsDir, job.url, relativePath, err)
						preserved, preservedErr := preservePreviousDocument(snapshotRoot, job.url, relativePath, previous)
						if preservedErr == nil {
							failure.PreservedExisting = true
							mu.Lock()
							results[job.index] = preserved
							succeeded[job.index] = true
							failures = append(failures, failure)
							mu.Unlock()
							recordCompletion()
							continue
						}
						if previous.Path != "" {
							failure.Error = fmt.Sprintf("%s (failed to preserve previous copy: %v)", failure.Error, preservedErr)
						}
						recordFailure(job.url, failure)
						continue
					}

					mu.Lock()
					results[job.index] = result
					succeeded[job.index] = true
					mu.Unlock()
					recordCompletion()
				}
			}
		}()
	}

enqueueLoop:
	for index, docURL := range docURLs {
		select {
		case <-ctx.Done():
			break enqueueLoop
		case jobs <- job{index: index, url: docURL}:
		}
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

func preservePreviousDocument(snapshotRoot string, rawURL string, relativePath string, previous manifestEntry) (fetchResult, error) {
	if previous.Path == "" {
		return fetchResult{}, errors.New("no previous snapshot entry")
	}

	previousPath := previous.Path
	if previousPath == "" {
		previousPath = filepath.ToSlash(relativePath)
	}

	localPath, sha256Value, bytesCount, err := summarizeExistingFile(snapshotRoot, previousPath)
	if err != nil {
		return fetchResult{}, err
	}
	if err := validateCachedSummary(previousPath, sha256Value, bytesCount, previous); err != nil {
		return fetchResult{}, err
	}

	return fetchResult{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}, nil
}

func fetchDocument(
	ctx context.Context,
	client *http.Client,
	policy *urlPolicy,
	spoolDir string,
	snapshotRoot string,
	rawURL string,
	relativePath string,
	previous manifestEntry,
) (fetchResult, error) {
	if err := policy.Validate(rawURL); err != nil {
		return fetchResult{}, err
	}

	requireMarkdown, err := isMarkdownURL(rawURL)
	if err != nil {
		return fetchResult{}, err
	}
	validators := fetchValidators{
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}

	attempts := 1
	if requireMarkdown {
		attempts = markdownFetchAttempts
	}

	for attempt := 0; attempt < attempts; attempt++ {
		response, err := fetchURL(ctx, client, rawURL, spoolDir, validators)
		if err != nil {
			return fetchResult{}, err
		}

		if response.NotModified {
			cachedResult, cacheErr := loadCachedDocument(snapshotRoot, rawURL, relativePath, previous, response)
			if cacheErr == nil {
				return cachedResult, nil
			}

			response, err = fetchURL(ctx, client, rawURL, spoolDir, fetchValidators{})
			if err != nil {
				return fetchResult{}, fmt.Errorf("%v (after cache-miss refetch failed: %w)", cacheErr, err)
			}
		}

		if requireMarkdown {
			if err := ensureMarkdownResponse(response.Status, response.ContentType, response.Headers, response.Sniff, response.BodyPath); err != nil {
				var unexpected *unexpectedContentError
				if errors.As(err, &unexpected) && attempt+1 < attempts {
					cleanupSpoolFile(response.BodyPath)
					if sleepErr := retrySleepWithJitter(ctx, attempt); sleepErr != nil {
						return fetchResult{}, sleepErr
					}
					continue
				}
				return fetchResult{}, err
			}
		}

		return fetchResult{
			URL:            rawURL,
			RelativePath:   relativePath,
			LocalPath:      response.BodyPath,
			SHA256:         response.SHA256,
			Bytes:          response.Bytes,
			LastModifiedAt: response.LastModifiedAt,
			ETag:           response.ETag,
		}, nil
	}

	return fetchResult{}, fmt.Errorf("failed to fetch %s", rawURL)
}

func loadCachedDocument(snapshotRoot string, rawURL string, relativePath string, previous manifestEntry, response httpFetchResponse) (fetchResult, error) {
	localPath, sha256Value, bytesCount, err := summarizeExistingFile(snapshotRoot, filepath.ToSlash(relativePath))
	if err != nil {
		return fetchResult{}, err
	}
	if err := validateCachedSummary(filepath.ToSlash(relativePath), sha256Value, bytesCount, previous); err != nil {
		return fetchResult{}, err
	}

	return fetchResult{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: coalesceValidator(response.LastModifiedAt, previous.LastModifiedAt),
		ETag:           coalesceValidator(response.ETag, previous.ETag),
	}, nil
}

func validateCachedSummary(relativePath string, sha256Value string, bytesCount int64, previous manifestEntry) error {
	if previous.SHA256 != "" && previous.SHA256 != sha256Value {
		return fmt.Errorf("cached file %s does not match previous manifest hash", relativePath)
	}
	if previous.Bytes > 0 && previous.Bytes != bytesCount {
		return fmt.Errorf("cached file %s size does not match previous manifest", relativePath)
	}
	return nil
}

func fetchURL(ctx context.Context, client *http.Client, rawURL string, spoolDir string, validators fetchValidators) (response httpFetchResponse, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return httpFetchResponse{}, err
	}

	req.Header.Set("User-Agent", buildUserAgent())
	req.Header.Set("Accept", "text/plain, text/markdown, text/*;q=0.9, */*;q=0.1")
	if ifNoneMatch := normalizeETag(validators.ETag); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifModifiedSince := ifModifiedSinceHeader(validators.LastModifiedAt); ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := client.Do(req)
	if err != nil {
		return httpFetchResponse{}, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()

	response = httpFetchResponse{
		Status:         resp.Status,
		ContentType:    strings.TrimSpace(resp.Header.Get("Content-Type")),
		Headers:        copyHeader(resp.Header),
		LastModifiedAt: normalizeLastModified(resp.Header.Get("Last-Modified")),
		ETag:           normalizeETag(resp.Header.Get("ETag")),
		NotModified:    resp.StatusCode == http.StatusNotModified,
	}

	if resp.StatusCode == http.StatusNotModified {
		response.LastModifiedAt = coalesceValidator(response.LastModifiedAt, validators.LastModifiedAt)
		response.ETag = coalesceValidator(response.ETag, validators.ETag)
		return response, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpFetchResponse{}, fmt.Errorf("unexpected HTTP %s", resp.Status)
	}

	spoolFile, err := os.CreateTemp(spoolDir, "fetch-*")
	if err != nil {
		return httpFetchResponse{}, fmt.Errorf("create spool file: %w", err)
	}

	cleanupOnError := true
	defer func() {
		_ = spoolFile.Close()
		if cleanupOnError {
			_ = os.Remove(spoolFile.Name())
		}
	}()

	hasher := sha256.New()
	capture := &prefixCaptureWriter{limit: htmlSniffBytes}

	written, err := io.Copy(io.MultiWriter(spoolFile, hasher, capture), resp.Body)
	if err != nil {
		return httpFetchResponse{}, fmt.Errorf("stream response body: %w", err)
	}

	response.BodyPath = spoolFile.Name()
	response.Sniff = append([]byte(nil), capture.buf...)
	response.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	response.Bytes = written

	cleanupOnError = false
	return response, nil
}

func ensureMarkdownResponse(status string, contentType string, headers map[string][]string, sniff []byte, bodyPath string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return &unexpectedContentError{
			message:     fmt.Sprintf("expected markdown response but received %s", mediaType),
			status:      status,
			contentType: contentType,
			headers:     headers,
			sniff:       append([]byte(nil), sniff...),
			bodyPath:    bodyPath,
		}
	}

	if looksLikeHTMLDocument(sniff) {
		return &unexpectedContentError{
			message:     "expected markdown response but received HTML document",
			status:      status,
			contentType: contentType,
			headers:     headers,
			sniff:       append([]byte(nil), sniff...),
			bodyPath:    bodyPath,
		}
	}

	return nil
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

func looksLikeHTMLDocument(body []byte) bool {
	trimmed := normalizeHTMLSniff(body)
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<head") ||
		strings.HasPrefix(lower, "<body")
}

func normalizeHTMLSniff(body []byte) string {
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

func sleepWithJitter(ctx context.Context, attempt int) error {
	base := 250 * time.Millisecond
	wait := base * time.Duration(1<<attempt)
	// #nosec G404 -- retry jitter only needs non-cryptographic randomness.
	jitter := time.Duration(rand.IntN(125)) * time.Millisecond

	timer := time.NewTimer(wait + jitter)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func summarizeExistingFile(root string, relativePath string) (localPath string, sha256Value string, bytesCount int64, err error) {
	localPath, err = safeJoin(root, relativePath)
	if err != nil {
		return "", "", 0, err
	}

	// #nosec G304 -- localPath is anchored to snapshotRoot via safeJoin.
	file, err := os.Open(localPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("read cached file %s: %w", relativePath, err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close cached file %s: %w", relativePath, closeErr)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(hasher, file)
	if err != nil {
		return "", "", 0, fmt.Errorf("hash cached file %s: %w", relativePath, err)
	}

	return localPath, hex.EncodeToString(hasher.Sum(nil)), written, nil
}

func safeJoin(root string, relativePath string) (string, error) {
	if root == "" {
		return "", errors.New("missing root directory")
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root directory: %w", err)
	}

	cleanRelative := filepath.Clean(filepath.FromSlash(relativePath))
	if filepath.IsAbs(cleanRelative) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", relativePath)
	}

	targetPath := filepath.Join(absoluteRoot, cleanRelative)
	relativeToRoot, err := filepath.Rel(absoluteRoot, targetPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", relativePath, err)
	}
	if relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes snapshot root: %s", relativePath)
	}

	return targetPath, nil
}

func cleanupSpoolFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

func buildFetchFailure(diagnosticsDir string, rawURL string, relativePath string, err error) fetchFailure {
	failure := fetchFailure{URL: rawURL, Error: err.Error()}

	var unexpected *unexpectedContentError
	if errors.As(err, &unexpected) {
		diagnosticPath, diagnosticErr := writeUnexpectedContentDiagnostic(diagnosticsDir, rawURL, relativePath, unexpected)
		if diagnosticErr != nil {
			failure.Error = fmt.Sprintf("%s (failed to write diagnostic: %v)", failure.Error, diagnosticErr)
		} else {
			failure.DiagnosticPath = diagnosticPath
		}
	}

	return failure
}

func writeUnexpectedContentDiagnostic(diagnosticsDir string, rawURL string, relativePath string, unexpected *unexpectedContentError) (string, error) {
	if diagnosticsDir == "" {
		return "", nil
	}

	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(unexpected.contentType, ";")[0]))
	bodyExtension := ".txt"
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" || looksLikeHTMLDocument(unexpected.sniff) {
		bodyExtension = ".html"
	}

	relativePath = filepath.ToSlash(relativePath)
	bodyRelativePath := relativePath + ".unexpected-content" + bodyExtension
	metaRelativePath := relativePath + ".unexpected-content.json"
	bodyPath := filepath.Join(diagnosticsDir, filepath.FromSlash(bodyRelativePath))
	metaPath := filepath.Join(diagnosticsDir, filepath.FromSlash(metaRelativePath))

	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o750); err != nil {
		return "", fmt.Errorf("create diagnostics directory: %w", err)
	}
	if err := copyFile(unexpected.bodyPath, bodyPath); err != nil {
		return "", fmt.Errorf("write diagnostic body: %w", err)
	}

	metadata := struct {
		URL          string              `json:"url"`
		RelativePath string              `json:"relative_path"`
		Error        string              `json:"error"`
		Status       string              `json:"status,omitempty"`
		ContentType  string              `json:"content_type,omitempty"`
		Headers      map[string][]string `json:"headers,omitempty"`
		BodyPath     string              `json:"body_path"`
	}{
		URL:          rawURL,
		RelativePath: relativePath,
		Error:        unexpected.message,
		Status:       unexpected.status,
		ContentType:  unexpected.contentType,
		Headers:      unexpected.headers,
		BodyPath:     bodyRelativePath,
	}

	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal diagnostic metadata: %w", err)
	}
	metadataBytes = append(metadataBytes, '\n')

	if err := os.WriteFile(metaPath, metadataBytes, 0o600); err != nil {
		return "", fmt.Errorf("write diagnostic metadata: %w", err)
	}

	return metaRelativePath, nil
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

func buildDiagnosticManifest(sourceURL string, sourcePath string, source *fetchResult, documents []fetchResult, skipped []skippedEntry, failures []fetchFailure) manifest {
	manifestData := manifest{
		SourceURL:     sourceURL,
		SourcePath:    filepath.ToSlash(sourcePath),
		DocumentCount: len(documents),
		SkippedCount:  len(skipped),
		Failures:      append([]fetchFailure(nil), failures...),
	}
	if source != nil {
		manifestData.SourceSHA256 = source.SHA256
		manifestData.SourceLastModifiedAt = source.LastModifiedAt
		manifestData.SourceETag = source.ETag
	}
	if len(skipped) > 0 {
		manifestData.Skipped = append([]skippedEntry(nil), skipped...)
	}
	if len(documents) > 0 {
		manifestData.Documents = make([]manifestEntry, 0, len(documents))
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
	}
	return manifestData
}

func writeDiagnosticManifest(manifestPath string, manifestData manifest) {
	if manifestPath == "" {
		return
	}
	if err := writeManifest(manifestPath, manifestData); err != nil {
		log.Printf("Failed to write diagnostics manifest %s: %v", manifestPath, err)
	}
}

func writeManifest(manifestPath string, manifestData manifest) error {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}

	manifestBytes, err := json.MarshalIndent(manifestData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}

func stageOutput(outputDir string, source fetchResult, documents []fetchResult) error {
	parentDir := filepath.Dir(outputDir)
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return fmt.Errorf("create directory for %s: %w", targetPath, err)
	}

	if err := copyFile(result.LocalPath, targetPath); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	return nil
}

func copyFile(sourcePath string, targetPath string) (err error) {
	// #nosec G304 -- sourcePath is a local spool or cached snapshot path produced by the crawler.
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}

	// #nosec G304 -- targetPath is a staged output path rooted under a temp directory controlled by the crawler.
	targetFile, err := os.Create(targetPath)
	if err != nil {
		_ = sourceFile.Close()
		return err
	}

	success := false
	defer func() {
		if !success {
			_ = targetFile.Close()
			_ = sourceFile.Close()
			_ = os.Remove(targetPath)
		}
	}()

	if _, err := io.Copy(targetFile, sourceFile); err != nil {
		return err
	}
	if err := sourceFile.Close(); err != nil {
		return err
	}
	if err := targetFile.Close(); err != nil {
		return err
	}
	// #nosec G302 -- tracked snapshot files are intended to remain world-readable in the checked-out repo.
	if err := os.Chmod(targetPath, 0o644); err != nil {
		return err
	}

	success = true
	return nil
}

func replaceDir(tempDir string, outputDir string) error {
	backupDir := outputDir + ".bak"

	outputExists := pathExists(outputDir)
	backupExists := pathExists(backupDir)

	if backupExists && !outputExists {
		if err := os.Rename(backupDir, outputDir); err != nil {
			return fmt.Errorf("restore backup output: %w", err)
		}
		outputExists = true
		backupExists = false
	}

	if backupExists && outputExists {
		if err := os.RemoveAll(backupDir); err != nil {
			return fmt.Errorf("remove stale backup directory: %w", err)
		}
		backupExists = false
	}

	if outputExists {
		if err := os.Rename(outputDir, backupDir); err != nil {
			return fmt.Errorf("backup existing output: %w", err)
		}
		backupExists = true
	}

	if err := os.Rename(tempDir, outputDir); err != nil {
		if backupExists {
			if restoreErr := os.Rename(backupDir, outputDir); restoreErr != nil {
				return fmt.Errorf("activate new output: %w (restore backup: %v)", err, restoreErr)
			}
		}
		return fmt.Errorf("activate new output: %w", err)
	}

	if backupExists {
		if err := os.RemoveAll(backupDir); err != nil {
			log.Printf("Warning: failed to remove backup directory %s: %v", backupDir, err)
		}
	}

	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
