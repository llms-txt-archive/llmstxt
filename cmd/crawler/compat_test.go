package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	apppkg "claudecodedocs/internal/app"
	fetchpkg "claudecodedocs/internal/fetch"
	linkspkg "claudecodedocs/internal/links"
	manifestpkg "claudecodedocs/internal/manifest"
	policypkg "claudecodedocs/internal/policy"
	stagepkg "claudecodedocs/internal/stage"
)

const (
	layoutRoot        = linkspkg.LayoutRoot
	layoutNested      = linkspkg.LayoutNested
	nonMarkdownReason = linkspkg.NonMarkdownReason
	htmlSniffBytes    = fetchpkg.HTMLSniffBytes
	progressLogEvery  = fetchpkg.ProgressLogEvery
)

type urlPolicy = policypkg.URLPolicy
type fetchResult = fetchpkg.Result
type fetchFailure = manifestpkg.FetchFailure
type manifest = manifestpkg.Manifest
type manifestEntry = manifestpkg.Entry
type skippedEntry = manifestpkg.SkippedEntry
type stageJournal = stagepkg.Journal

type unexpectedContentError struct {
	message     string
	status      string
	contentType string
	headers     map[string][]string
	sniff       []byte
	bodyPath    string
}

func (e *unexpectedContentError) Error() string {
	return e.message
}

var testStageOpts *stagepkg.Options

func toCompatUnexpected(err error) error {
	var unexpected *fetchpkg.UnexpectedContentError
	if !errors.As(err, &unexpected) {
		return err
	}

	return &unexpectedContentError{
		message:     unexpected.Message,
		status:      unexpected.Status,
		contentType: unexpected.ContentType,
		headers:     unexpected.Headers,
		sniff:       append([]byte(nil), unexpected.Sniff...),
		bodyPath:    unexpected.BodyPath,
	}
}

func toFetchUnexpected(err error) error {
	var unexpected *unexpectedContentError
	if !errors.As(err, &unexpected) {
		return err
	}

	return &fetchpkg.UnexpectedContentError{
		Message:     unexpected.message,
		Status:      unexpected.status,
		ContentType: unexpected.contentType,
		Headers:     unexpected.headers,
		Sniff:       append([]byte(nil), unexpected.sniff...),
		BodyPath:    unexpected.bodyPath,
	}
}

func extractLinks(body []byte) ([]string, error) {
	return linkspkg.Extract(body)
}

func partitionDocumentURLs(allLinks []string) ([]string, []string, []skippedEntry, error) {
	return linkspkg.Partition(allLinks)
}

func isIndex(rawURL string) bool {
	return linkspkg.IsIndex(rawURL)
}

func isMarkdownURL(rawURL string) (bool, error) {
	return linkspkg.IsMarkdown(rawURL)
}

func newURLPolicy(sourceURL string, allowedHostsCSV string) (*urlPolicy, error) {
	return policypkg.NewURLPolicy(sourceURL, allowedHostsCSV)
}

func validateResolvedIP(ip net.IP) error {
	return policypkg.ValidateResolvedIP(ip)
}

func newHTTPClient(timeout time.Duration, urlPolicy *urlPolicy) *http.Client {
	return policypkg.NewHTTPClient(timeout, urlPolicy)
}

func relativePathForURL(rawURL string, layout string) (string, error) {
	return linkspkg.RelativePath(rawURL, layout)
}

func sourcePathForLayout(layout string) string {
	return linkspkg.SourcePath(layout)
}

func writeManifest(manifestPath string, manifestData manifest) error {
	return manifestpkg.Write(manifestPath, &manifestData)
}

func loadManifest(manifestPath string) (*manifest, error) {
	return manifestpkg.Load(manifestPath)
}

func normalizeLastModified(value string) string {
	return fetchpkg.NormalizeLastModified(value)
}

func ifModifiedSinceHeader(lastModifiedAt string) string {
	return fetchpkg.IfModifiedSinceHeader(lastModifiedAt)
}

func normalizeETag(value string) string {
	return fetchpkg.NormalizeETag(value)
}

func ensureMarkdownResponse(status string, contentType string, headers map[string][]string, sniff []byte, bodyPath string) error {
	return toCompatUnexpected(fetchpkg.EnsureMarkdownResponse(status, contentType, headers, sniff, bodyPath))
}

func writeUnexpectedContentDiagnostic(diagnosticsDir string, rawURL string, relativePath string, unexpected *unexpectedContentError) (string, error) {
	//nolint:errorlint // type is guaranteed by toFetchUnexpected
	return fetchpkg.WriteUnexpectedContentDiagnostic(diagnosticsDir, rawURL, relativePath, toFetchUnexpected(unexpected).(*fetchpkg.UnexpectedContentError))
}

func buildDiagnosticManifest(sourceURL string, sourcePath string, source *fetchResult, documents []fetchResult, skipped []skippedEntry, failures []fetchFailure) manifest {
	return apppkg.BuildDiagnosticManifest(sourceURL, sourcePath, source, documents, skipped, failures)
}

func fetchDocument(ctx context.Context, client *http.Client, urlPolicy *urlPolicy, spoolDir string, snapshotRoot string, rawURL string, relativePath string, previous manifestEntry) (fetchResult, error) {
	return fetchpkg.Document(ctx, client, urlPolicy, spoolDir, snapshotRoot, rawURL, relativePath, previous, nil)
}

func preservePreviousDocument(snapshotRoot string, rawURL string, relativePath string, previous manifestEntry) (fetchResult, error) {
	return fetchpkg.PreservePreviousDocument(snapshotRoot, rawURL, relativePath, previous)
}

func hashBytes(body []byte) string {
	return fetchpkg.HashBytes(body)
}

func safeJoin(root string, relativePath string) (string, error) {
	return fetchpkg.SafeJoin(root, relativePath)
}

func fetchDocuments(ctx context.Context, client *http.Client, urlPolicy *urlPolicy, layout string, diagnosticsDir string, spoolDir string, snapshotRoot string, docURLs []string, concurrency int, previousDocuments map[string]manifestEntry) ([]fetchResult, []fetchFailure) {
	return fetchpkg.Documents(ctx, docURLs, fetchpkg.Options{
		Client:            client,
		URLPolicy:         urlPolicy,
		Layout:            layout,
		DiagnosticsDir:    diagnosticsDir,
		SpoolDir:          spoolDir,
		SnapshotRoot:      snapshotRoot,
		Concurrency:       concurrency,
		PreviousDocuments: previousDocuments,
	})
}

func replaceDir(tempDir string, outputDir string) error {
	return stagepkg.ReplaceDir(tempDir, outputDir, testStageOpts)
}

func reconcileStageState(outputDir string) error {
	return stagepkg.ReconcileState(outputDir, testStageOpts)
}

func writeStageCompletionMarker(root string) error {
	return stagepkg.WriteCompletionMarker(root)
}

func writeStageJournal(outputDir string, journal stageJournal) error {
	return stagepkg.WriteJournal(outputDir, journal)
}

func stageJournalPath(outputDir string) string {
	return stagepkg.JournalPath(outputDir)
}

func stageCompletionMarkerPath(root string) string {
	return stagepkg.CompletionMarkerPath(root)
}
