// Package app orchestrates the end-to-end llms.txt sync workflow.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/llms-txt-archive/llmstxt/internal/fetch"
	"github.com/llms-txt-archive/llmstxt/internal/links"
	"github.com/llms-txt-archive/llmstxt/internal/logutil"
	"github.com/llms-txt-archive/llmstxt/internal/manifest"
	"github.com/llms-txt-archive/llmstxt/internal/policy"
	"github.com/llms-txt-archive/llmstxt/internal/stage"

	"golang.org/x/time/rate"
)

// maxNestedIndexes caps the BFS traversal to prevent runaway crawling
// if an index graph contains a cycle that escapes URL deduplication.
const maxNestedIndexes = 50

// Config holds the CLI flags and settings for a sync run.
type Config struct {
	SourceURL            string
	OutputDir            string
	Layout               string
	PreviousManifestPath string
	ManifestOut          string
	DiagnosticsDir       string
	AllowedHostsCSV      string
	ArchiveRoot          string
	RateLimit            float64
	Timeout              time.Duration
	Concurrency          int
	Logger               *slog.Logger
}

func (c Config) logger() *slog.Logger { return logutil.Default(c.Logger) }

// PartialSyncError reports documents that failed to fetch while others succeeded.
// Callers can detect this with errors.As to distinguish partial failure from total failure.
type PartialSyncError struct {
	Failures []manifest.FetchFailure
}

func (e *PartialSyncError) Error() string {
	if len(e.Failures) == 0 {
		return "partial sync"
	}

	lines := make([]string, 0, len(e.Failures)+1)
	lines = append(lines, fmt.Sprintf("%d fetches failed", len(e.Failures)))
	for _, failure := range e.Failures {
		lines = append(lines, fmt.Sprintf("- %s: %s", failure.URL, failure.Error))
	}

	return strings.Join(lines, "\n")
}

func resultToEntry(r fetch.Result) manifest.Entry {
	return manifest.Entry{
		URL:            r.URL,
		Path:           filepath.ToSlash(r.RelativePath),
		SHA256:         r.SHA256,
		Bytes:          r.Bytes,
		LastModifiedAt: r.LastModifiedAt,
		ETag:           r.ETag,
	}
}

// BuildManifest assembles a manifest from the source and document fetch results.
func BuildManifest(source fetch.Result, documents []fetch.Result, skipped []manifest.SkippedEntry, failures []manifest.FetchFailure, discoveredIndexes []fetch.Result) manifest.Manifest {
	m := manifest.Manifest{
		SourceURL:            source.URL,
		SourcePath:           filepath.ToSlash(source.RelativePath),
		SourceSHA256:         source.SHA256,
		SourceLastModifiedAt: source.LastModifiedAt,
		SourceETag:           source.ETag,
		DocumentCount:        len(documents),
		SkippedCount:         len(skipped),
		Documents:            make([]manifest.Entry, 0, len(documents)),
	}

	if len(skipped) > 0 {
		m.Skipped = append(m.Skipped, skipped...)
	}
	if len(failures) > 0 {
		m.Failures = append(m.Failures, failures...)
	}

	for _, document := range documents {
		m.Documents = append(m.Documents, resultToEntry(document))
	}

	if len(discoveredIndexes) > 0 {
		sources := make([]manifest.SourceEntry, 0, len(discoveredIndexes))
		for _, idx := range discoveredIndexes {
			sources = append(sources, manifest.SourceEntry{
				URL:            idx.URL,
				Path:           filepath.ToSlash(idx.RelativePath),
				SHA256:         idx.SHA256,
				LastModifiedAt: idx.LastModifiedAt,
				ETag:           idx.ETag,
			})
		}
		m.Sources = sources
	}

	return m
}

// BuildDiagnosticManifest assembles a manifest for error diagnostics, tolerating nil source results.
func BuildDiagnosticManifest(sourceURL string, sourcePath string, source *fetch.Result, documents []fetch.Result, skipped []manifest.SkippedEntry, failures []manifest.FetchFailure) manifest.Manifest {
	m := manifest.Manifest{
		SourceURL:     sourceURL,
		SourcePath:    filepath.ToSlash(sourcePath),
		DocumentCount: len(documents),
		SkippedCount:  len(skipped),
		Failures:      append([]manifest.FetchFailure(nil), failures...),
	}
	if source != nil {
		m.SourceSHA256 = source.SHA256
		m.SourceLastModifiedAt = source.LastModifiedAt
		m.SourceETag = source.ETag
	}
	if len(skipped) > 0 {
		m.Skipped = append([]manifest.SkippedEntry(nil), skipped...)
	}
	if len(documents) > 0 {
		m.Documents = make([]manifest.Entry, 0, len(documents))
		for _, document := range documents {
			m.Documents = append(m.Documents, resultToEntry(document))
		}
	}
	return m
}

// WriteDiagnosticManifest writes a diagnostic manifest to disk, logging errors instead of returning them.
func WriteDiagnosticManifest(manifestPath string, m manifest.Manifest) {
	if manifestPath == "" {
		return
	}
	if err := manifest.Write(manifestPath, &m); err != nil {
		slog.Warn("failed to write diagnostics manifest", "path", manifestPath, "error", err)
	}
}

// DiscoveryConfig holds the dependencies for BFS index discovery.
type DiscoveryConfig struct {
	Client       *http.Client
	URLPolicy    *policy.URLPolicy
	SpoolDir     string
	ArchiveRoot  string
	Layout       string
	PreviousDocs map[string]manifest.Entry
	Logger       *slog.Logger
}

func (c DiscoveryConfig) logger() *slog.Logger { return logutil.Default(c.Logger) }

// DiscoveryResult holds the output of BFS index discovery.
type DiscoveryResult struct {
	DocURLs      []string
	Skipped      []manifest.SkippedEntry
	IndexResults []fetch.Result
}

// normalizeIndexURL strips the fragment from a URL for dedup purposes.
func normalizeIndexURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.Fragment = ""
	return parsed.String()
}

func skippedEntry(rawURL, reason string, err error) manifest.SkippedEntry {
	return manifest.SkippedEntry{
		URL:    rawURL,
		Reason: fmt.Sprintf("%s: %v", reason, err),
	}
}

type indexResult struct {
	DocURLs      []string
	ChildIndexes []string
	Skipped      []manifest.SkippedEntry
	FetchResult  *fetch.Result
}

// processIndex fetches and parses a single nested index, returning discovered docs and child indexes.
// Errors that prevent processing a single index are returned as skipped entries, not as errors.
func processIndex(ctx context.Context, cfg DiscoveryConfig, rawURL string) (*indexResult, error) {
	if err := cfg.URLPolicy.Validate(rawURL); err != nil {
		return &indexResult{Skipped: []manifest.SkippedEntry{skippedEntry(rawURL, "policy", err)}}, nil
	}

	relativePath := filepath.Join("sources", links.SourcePath(cfg.Layout))
	if relPath, relErr := links.RelativePath(rawURL, cfg.Layout); relErr == nil {
		relativePath = relPath
	}

	previous := manifest.Entry{}
	if cfg.PreviousDocs != nil {
		if prev, ok := cfg.PreviousDocs[rawURL]; ok {
			previous = prev
		}
	}

	fetchResult, fetchErr := fetch.Document(ctx, rawURL, relativePath, previous, fetch.DocumentConfig{
		ClientConfig: fetch.ClientConfig{
			Client:      cfg.Client,
			URLPolicy:   cfg.URLPolicy,
			SpoolDir:    cfg.SpoolDir,
			ArchiveRoot: cfg.ArchiveRoot,
		},
	})
	if fetchErr != nil {
		cfg.logger().Warn("skipping nested index", "url", rawURL, "error", fetchErr)
		return &indexResult{Skipped: []manifest.SkippedEntry{skippedEntry(rawURL, "fetch failed", fetchErr)}}, nil
	}

	body, readErr := os.ReadFile(fetchResult.LocalPath)
	if readErr != nil {
		cfg.logger().Warn("skipping nested index", "url", rawURL, "error", readErr)
		return &indexResult{Skipped: []manifest.SkippedEntry{skippedEntry(rawURL, "read failed", readErr)}}, nil
	}

	childLinks, extractErr := links.Extract(body)
	if extractErr != nil {
		cfg.logger().Warn("skipping nested index", "url", rawURL, "error", extractErr)
		return &indexResult{Skipped: []manifest.SkippedEntry{skippedEntry(rawURL, "no links", extractErr)}}, nil
	}

	docs, indexes, partSkipped, partErr := links.Partition(childLinks)
	if partErr != nil {
		cfg.logger().Warn("skipping nested index", "url", rawURL, "error", partErr)
		return &indexResult{Skipped: []manifest.SkippedEntry{skippedEntry(rawURL, "partition error", partErr)}}, nil
	}

	return &indexResult{
		DocURLs:      docs,
		ChildIndexes: indexes,
		Skipped:      partSkipped,
		FetchResult:  &fetchResult,
	}, nil
}

// DiscoverDocuments performs BFS discovery of nested llms.txt indexes and their linked documents.
func DiscoverDocuments(
	ctx context.Context,
	primarySourceURL string,
	initialLinks []string,
	cfg DiscoveryConfig,
) (*DiscoveryResult, error) {
	docURLs, indexQueue, skipped, err := links.Partition(initialLinks)
	if err != nil {
		return nil, err
	}

	// visitedIndexes and seenDocs are accessed only from this goroutine (sequential BFS).
	// Do NOT access them from worker goroutines without adding synchronization.
	visitedIndexes := map[string]bool{normalizeIndexURL(primarySourceURL): true}
	seenDocs := make(map[string]bool, len(docURLs))
	for _, u := range docURLs {
		seenDocs[normalizeIndexURL(u)] = true
	}

	queue := make([]string, 0, len(indexQueue))
	queue = append(queue, indexQueue...)

	var indexResults []fetch.Result

	for i := 0; i < len(queue); i++ {
		if ctx.Err() != nil {
			break
		}

		if len(indexResults) >= maxNestedIndexes {
			cfg.logger().Warn("nested index cap reached", "limit", maxNestedIndexes)
			break
		}

		rawURL := queue[i]
		normalized := normalizeIndexURL(rawURL)
		if visitedIndexes[normalized] {
			continue
		}
		visitedIndexes[normalized] = true

		idx, err := processIndex(ctx, cfg, rawURL)
		if err != nil {
			return nil, err
		}
		if idx.FetchResult != nil {
			indexResults = append(indexResults, *idx.FetchResult)
		}

		for _, u := range idx.DocURLs {
			if norm := normalizeIndexURL(u); !seenDocs[norm] {
				seenDocs[norm] = true
				docURLs = append(docURLs, u)
			}
		}
		skipped = append(skipped, idx.Skipped...)
		for _, u := range idx.ChildIndexes {
			if !visitedIndexes[normalizeIndexURL(u)] {
				queue = append(queue, u)
			}
		}
	}

	return &DiscoveryResult{
		DocURLs:      docURLs,
		Skipped:      skipped,
		IndexResults: indexResults,
	}, nil
}

// Run executes the full sync: fetch the llms.txt source, download linked documents, and stage the output.
func Run(ctx context.Context, cfg Config) error {
	urlPolicy, err := policy.NewURLPolicy(cfg.SourceURL, cfg.AllowedHostsCSV)
	if err != nil {
		return err
	}

	if err := urlPolicy.Validate(cfg.SourceURL); err != nil {
		return fmt.Errorf("validate source URL: %w", err)
	}

	client := policy.NewHTTPClient(cfg.Timeout, urlPolicy)

	spoolDir, err := os.MkdirTemp("", ".llmstxt-fetch-*")
	if err != nil {
		return fmt.Errorf("create fetch spool directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(spoolDir)
	}()

	previousManifest, err := manifest.Load(cfg.PreviousManifestPath)
	if err != nil {
		return err
	}

	previousDocs := manifest.PreviousDocumentsByURL(previousManifest)
	sourcePath := links.SourcePath(cfg.Layout)
	sourcePrevious := manifest.PreviousSourceEntry(previousManifest, sourcePath)

	// Diagnostic state accumulates context for error-path manifest writes.
	var (
		diagSource   *fetch.Result
		diagDocs     []fetch.Result
		diagSkipped  []manifest.SkippedEntry
		diagFailures []manifest.FetchFailure
		runErr       error
	)
	defer func() {
		if runErr != nil {
			WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(
				cfg.SourceURL, sourcePath, diagSource, diagDocs, diagSkipped, diagFailures,
			))
		}
	}()

	addFailure := func(url string, err error) {
		diagFailures = append(diagFailures, manifest.FetchFailure{URL: url, Error: err.Error()})
	}

	cc := fetch.ClientConfig{
		Client:      client,
		URLPolicy:   urlPolicy,
		SpoolDir:    spoolDir,
		ArchiveRoot: cfg.ArchiveRoot,
	}

	sourceResult, err := fetch.Document(ctx, cfg.SourceURL, sourcePath, sourcePrevious, fetch.DocumentConfig{
		ClientConfig: cc,
	})
	if err != nil {
		diagFailures = []manifest.FetchFailure{fetch.BuildFetchFailure(cfg.DiagnosticsDir, cfg.SourceURL, sourcePath, err)}
		runErr = fmt.Errorf("fetch %s: %w", cfg.SourceURL, err)
		return runErr
	}
	diagSource = &sourceResult

	sourceBody, err := os.ReadFile(sourceResult.LocalPath)
	if err != nil {
		addFailure(cfg.SourceURL, fmt.Errorf("read fetched llms.txt: %w", err))
		runErr = fmt.Errorf("read fetched llms.txt: %w", err)
		return runErr
	}

	extractedLinks, err := links.Extract(sourceBody)
	if err != nil {
		addFailure(cfg.SourceURL, err)
		runErr = err
		return runErr
	}

	discovery, err := DiscoverDocuments(ctx, cfg.SourceURL, extractedLinks, DiscoveryConfig{
		Client:       client,
		URLPolicy:    urlPolicy,
		SpoolDir:     spoolDir,
		ArchiveRoot:  cfg.ArchiveRoot,
		Layout:       cfg.Layout,
		PreviousDocs: previousDocs,
		Logger:       cfg.Logger,
	})
	if err != nil {
		addFailure(cfg.SourceURL, err)
		runErr = err
		return runErr
	}

	docURLs := discovery.DocURLs
	diagSkipped = discovery.Skipped

	if len(discovery.IndexResults) > 0 {
		cfg.logger().Info("discovered nested indexes", "count", len(discovery.IndexResults))
	}

	var limiter *rate.Limiter
	if cfg.RateLimit > 0 {
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), int(cfg.RateLimit)+1)
	}

	documents, failures := fetch.Documents(ctx, docURLs, fetch.Options{
		ClientConfig:   cc,
		Layout:         cfg.Layout,
		DiagnosticsDir: cfg.DiagnosticsDir,
		Concurrency:    cfg.Concurrency,
		RateLimiter:    limiter,
		PreviousDocs:   previousDocs,
		Logger:         cfg.Logger,
	})
	diagDocs = documents
	diagFailures = failures

	if err := ctx.Err(); err != nil {
		runErr = err
		return runErr
	}

	m := BuildManifest(sourceResult, documents, diagSkipped, failures, discovery.IndexResults)

	allResults := make([]fetch.Result, 0, 1+len(documents)+len(discovery.IndexResults))
	allResults = append(allResults, sourceResult)
	allResults = append(allResults, documents...)
	allResults = append(allResults, discovery.IndexResults...)

	stageFiles := make([]stage.FileEntry, len(allResults))
	for i, r := range allResults {
		stageFiles[i] = stage.FileEntry{RelativePath: r.RelativePath, LocalPath: r.LocalPath}
	}
	if err := stage.Output(cfg.OutputDir, stageFiles, nil); err != nil {
		diagFailures = append(append([]manifest.FetchFailure(nil), failures...), manifest.FetchFailure{
			URL:   cfg.SourceURL,
			Error: fmt.Sprintf("stage output: %v", err),
		})
		runErr = err
		return runErr
	}

	// Success path: no diagnostic manifest needed, clear runErr.
	if err := manifest.Write(cfg.ManifestOut, &m); err != nil {
		return err
	}

	cfg.logger().Info("sync complete",
		"documents", len(documents),
		"output", cfg.OutputDir,
		"skipped", len(diagSkipped),
		"failures", len(failures),
	)

	if len(failures) > 0 {
		return &PartialSyncError{Failures: failures}
	}

	return nil
}
