// Package app orchestrates the end-to-end llms.txt sync workflow.
package app

import (
	"context"
	"fmt"
	"log"
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
func BuildManifest(source fetch.Result, documents []fetch.Result, skipped []manifest.SkippedEntry, failures []manifest.FetchFailure) manifest.Manifest {
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

	sourceResult, err := fetch.FetchDocument(ctx, client, urlPolicy, spoolDir, cfg.SnapshotRoot, cfg.SourceURL, sourcePath, sourcePrevious)
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

	docURLs, skipped, err := links.Partition(extractedLinks)
	if err != nil {
		failures := []manifest.FetchFailure{{URL: cfg.SourceURL, Error: err.Error()}}
		WriteDiagnosticManifest(cfg.ManifestOut, BuildDiagnosticManifest(cfg.SourceURL, sourcePath, &sourceResult, nil, skipped, failures))
		return err
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

	manifestData := BuildManifest(sourceResult, documents, skipped, failures)
	if err := stage.StageOutput(cfg.OutputDir, sourceResult, documents); err != nil {
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
