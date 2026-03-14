// Package app orchestrates the end-to-end llms.txt sync workflow.
package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claudecodedocs/internal/fetch"
	"claudecodedocs/internal/links"
	"claudecodedocs/internal/manifest"
	"claudecodedocs/internal/policy"
	"claudecodedocs/internal/stage"
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
	SnapshotRoot         string
	Timeout              time.Duration
	Concurrency          int
}

// PartialSyncError reports documents that failed to fetch while others succeeded.
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

// BuildManifest assembles a manifest from the source and document fetch results.
func BuildManifest(source fetch.Result, documents []fetch.Result, skipped []manifest.SkippedEntry, failures []manifest.FetchFailure, discoveredIndexes []fetch.Result) manifest.Manifest {
	manifestData := manifest.Manifest{
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
		manifestData.Skipped = append(manifestData.Skipped, skipped...)
	}
	if len(failures) > 0 {
		manifestData.Failures = append(manifestData.Failures, failures...)
	}

	for _, document := range documents {
		manifestData.Documents = append(manifestData.Documents, manifest.Entry{
			URL:            document.URL,
			Path:           filepath.ToSlash(document.RelativePath),
			SHA256:         document.SHA256,
			Bytes:          document.Bytes,
			LastModifiedAt: document.LastModifiedAt,
			ETag:           document.ETag,
		})
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
		manifestData.Sources = sources
	}

	return manifestData
}

// BuildDiagnosticManifest assembles a manifest for error diagnostics, tolerating nil source results.
func BuildDiagnosticManifest(sourceURL string, sourcePath string, source *fetch.Result, documents []fetch.Result, skipped []manifest.SkippedEntry, failures []manifest.FetchFailure) manifest.Manifest {
	manifestData := manifest.Manifest{
		SourceURL:     sourceURL,
		SourcePath:    filepath.ToSlash(sourcePath),
		DocumentCount: len(documents),
		SkippedCount:  len(skipped),
		Failures:      append([]manifest.FetchFailure(nil), failures...),
	}
	if source != nil {
		manifestData.SourceSHA256 = source.SHA256
		manifestData.SourceLastModifiedAt = source.LastModifiedAt
		manifestData.SourceETag = source.ETag
	}
	if len(skipped) > 0 {
		manifestData.Skipped = append([]manifest.SkippedEntry(nil), skipped...)
	}
	if len(documents) > 0 {
		manifestData.Documents = make([]manifest.Entry, 0, len(documents))
		for _, document := range documents {
			manifestData.Documents = append(manifestData.Documents, manifest.Entry{
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

// WriteDiagnosticManifest writes a diagnostic manifest to disk, logging errors instead of returning them.
func WriteDiagnosticManifest(manifestPath string, manifestData manifest.Manifest) {
	if manifestPath == "" {
		return
	}
	if err := manifest.Write(manifestPath, &manifestData); err != nil {
		log.Printf("Failed to write diagnostics manifest %s: %v", manifestPath, err)
	}
}

// DiscoveryConfig holds the dependencies for BFS index discovery.
type DiscoveryConfig struct {
	Client       *http.Client
	URLPolicy    *policy.URLPolicy
	SpoolDir     string
	SnapshotRoot string
	Layout       string
	PreviousDocs map[string]manifest.Entry
}

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

func skipEntry(skipped *[]manifest.SkippedEntry, rawURL, reason string, err error) {
	*skipped = append(*skipped, manifest.SkippedEntry{
		URL:    rawURL,
		Reason: fmt.Sprintf("%s: %v", reason, err),
	})
}

// processIndex fetches and parses a single nested index, returning discovered docs and child indexes.
func processIndex(
	ctx context.Context,
	cfg DiscoveryConfig,
	rawURL string,
	skipped *[]manifest.SkippedEntry,
) (docURLs, childIndexes []string, childSkipped []manifest.SkippedEntry, result *fetch.Result, err error) {
	if err := cfg.URLPolicy.Validate(rawURL); err != nil {
		skipEntry(skipped, rawURL, "policy", err)
		return nil, nil, nil, nil, nil
	}

	relativePath := fmt.Sprintf("sources/%s", links.SourcePath(cfg.Layout))
	if relPath, relErr := links.RelativePath(rawURL, cfg.Layout); relErr == nil {
		relativePath = relPath
	}

	previous := manifest.Entry{}
	if cfg.PreviousDocs != nil {
		if prev, ok := cfg.PreviousDocs[rawURL]; ok {
			previous = prev
		}
	}

	fetchResult, fetchErr := fetch.FetchDocument(ctx, cfg.Client, cfg.URLPolicy, cfg.SpoolDir, cfg.SnapshotRoot, rawURL, relativePath, previous, nil)
	if fetchErr != nil {
		log.Printf("Skipping nested index %s: %v", rawURL, fetchErr)
		skipEntry(skipped, rawURL, "fetch failed", fetchErr)
		return nil, nil, nil, nil, nil
	}

	body, readErr := os.ReadFile(fetchResult.LocalPath)
	if readErr != nil {
		log.Printf("Skipping nested index %s: cannot read: %v", rawURL, readErr)
		skipEntry(skipped, rawURL, "read failed", readErr)
		return nil, nil, nil, nil, nil
	}

	childLinks, extractErr := links.Extract(body)
	if extractErr != nil {
		log.Printf("Skipping nested index %s: no links: %v", rawURL, extractErr)
		skipEntry(skipped, rawURL, "no links", extractErr)
		return nil, nil, nil, nil, nil
	}

	docs, indexes, partSkipped, partErr := links.Partition(childLinks)
	if partErr != nil {
		log.Printf("Skipping nested index %s: partition error: %v", rawURL, partErr)
		skipEntry(skipped, rawURL, "partition error", partErr)
		return nil, nil, nil, nil, nil
	}

	return docs, indexes, partSkipped, &fetchResult, nil
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

	queue := make([]string, len(indexQueue))
	copy(queue, indexQueue)

	var indexResults []fetch.Result

	for i := 0; i < len(queue); i++ {
		if ctx.Err() != nil {
			break
		}

		if len(indexResults) >= maxNestedIndexes {
			log.Printf("Nested index cap reached (%d); stopping BFS", maxNestedIndexes)
			break
		}

		rawURL := queue[i]
		normalized := normalizeIndexURL(rawURL)
		if visitedIndexes[normalized] {
			continue
		}
		visitedIndexes[normalized] = true

		childDocs, childIndexes, childSkipped, result, err := processIndex(ctx, cfg, rawURL, &skipped)
		if err != nil {
			return nil, err
		}
		if result != nil {
			indexResults = append(indexResults, *result)
		}

		for _, u := range childDocs {
			if norm := normalizeIndexURL(u); !seenDocs[norm] {
				seenDocs[norm] = true
				docURLs = append(docURLs, u)
			}
		}
		skipped = append(skipped, childSkipped...)
		for _, u := range childIndexes {
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

	spoolDir, err := os.MkdirTemp("", ".claudecodedocs-fetch-*")
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

	previousDocuments := manifest.PreviousDocumentsByURL(previousManifest)
	sourcePath := links.SourcePath(cfg.Layout)
	sourcePrevious := manifest.PreviousSourceEntry(previousManifest, sourcePath)

	sourceResult, err := fetch.FetchDocument(ctx, client, urlPolicy, spoolDir, cfg.SnapshotRoot, cfg.SourceURL, sourcePath, sourcePrevious, nil)
	if err != nil {
		failures := []manifest.FetchFailure{fetch.BuildFetchFailure(cfg.DiagnosticsDir, cfg.SourceURL, sourcePath, err)}
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, nil, nil, nil, failures))
		return fmt.Errorf("fetch %s: %w", cfg.SourceURL, err)
	}

	sourceBody, err := os.ReadFile(sourceResult.LocalPath)
	if err != nil {
		failures := []manifest.FetchFailure{{URL: cfg.SourceURL, Error: fmt.Sprintf("read fetched llms.txt: %v", err)}}
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, nil, nil, failures))
		return fmt.Errorf("read fetched llms.txt: %w", err)
	}

	extractedLinks, err := links.Extract(sourceBody)
	if err != nil {
		failures := []manifest.FetchFailure{{URL: cfg.SourceURL, Error: err.Error()}}
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, nil, nil, failures))
		return err
	}

	discovery, err := DiscoverDocuments(ctx, cfg.SourceURL, extractedLinks, DiscoveryConfig{
		Client:       client,
		URLPolicy:    urlPolicy,
		SpoolDir:     spoolDir,
		SnapshotRoot: cfg.SnapshotRoot,
		Layout:       cfg.Layout,
		PreviousDocs: previousDocuments,
	})
	if err != nil {
		failures := []manifest.FetchFailure{{URL: cfg.SourceURL, Error: err.Error()}}
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, nil, nil, failures))
		return err
	}

	docURLs := discovery.DocURLs
	skipped := discovery.Skipped

	if len(discovery.IndexResults) > 0 {
		log.Printf("Discovered %d nested llms.txt indexes", len(discovery.IndexResults))
	}

	documents, failures := fetch.FetchDocuments(ctx, docURLs, fetch.FetchOptions{
		Client:            client,
		URLPolicy:         urlPolicy,
		Layout:            cfg.Layout,
		DiagnosticsDir:    cfg.DiagnosticsDir,
		SpoolDir:          spoolDir,
		SnapshotRoot:      cfg.SnapshotRoot,
		Concurrency:       cfg.Concurrency,
		PreviousDocuments: previousDocuments,
	})
	if err := ctx.Err(); err != nil {
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, documents, skipped, failures))
		return err
	}

	manifestData := BuildManifest(sourceResult, documents, skipped, failures, discovery.IndexResults)

	allResults := make([]fetch.Result, 0, len(documents)+len(discovery.IndexResults))
	allResults = append(allResults, documents...)
	allResults = append(allResults, discovery.IndexResults...)
	if err := stage.StageOutput(cfg.OutputDir, sourceResult, allResults, nil); err != nil {
		failureWithStage := append([]manifest.FetchFailure(nil), failures...)
		failureWithStage = append(failureWithStage, manifest.FetchFailure{
			URL:   cfg.SourceURL,
			Error: fmt.Sprintf("stage output: %v", err),
		})
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, documents, skipped, failureWithStage))
		return err
	}

	if err := manifest.Write(cfg.ManifestOut, &manifestData); err != nil {
		return err
	}

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "%s\n", (&PartialSyncError{Failures: failures}).Error())
	}

	fmt.Printf(
		"Fetched %d markdown documents into %s (%d skipped non-markdown URLs, %d fetch failures)\n",
		len(documents),
		cfg.OutputDir,
		len(skipped),
		len(failures),
	)
	return nil
}
