package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claudecodedocs/internal/links"
	"claudecodedocs/internal/manifest"
	"claudecodedocs/internal/policy"
)

const (
	MarkdownFetchAttempts = 3
	HTMLSniffBytes        = 4096
	ProgressLogEvery      = 25
)

var RetrySleepWithJitter = sleepWithJitter

type Result struct {
	URL            string
	RelativePath   string
	LocalPath      string
	SHA256         string
	Bytes          int64
	LastModifiedAt string
	ETag           string
}

type UnexpectedContentError struct {
	Message     string
	Status      string
	ContentType string
	Headers     map[string][]string
	Sniff       []byte
	BodyPath    string
}

type Validators struct {
	LastModifiedAt string
	ETag           string
}

type HTTPResponse struct {
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

type PrefixCaptureWriter struct {
	Limit int
	Buf   []byte
}

func (e *UnexpectedContentError) Error() string {
	return e.Message
}

func (w *PrefixCaptureWriter) Write(p []byte) (int, error) {
	if remaining := w.Limit - len(w.Buf); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.Buf = append(w.Buf, p[:remaining]...)
	}
	return len(p), nil
}

func FetchDocuments(
	ctx context.Context,
	client *http.Client,
	urlPolicy *policy.URLPolicy,
	layout string,
	diagnosticsDir string,
	spoolDir string,
	snapshotRoot string,
	docURLs []string,
	concurrency int,
	previousDocuments map[string]manifest.Entry,
) ([]Result, []manifest.FetchFailure) {
	type job struct {
		index int
		url   string
	}

	results := make([]Result, len(docURLs))
	succeeded := make([]bool, len(docURLs))
	failures := make([]manifest.FetchFailure, 0)

	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		completed atomic.Int64
	)

	recordCompletion := func() {
		done := completed.Add(1)
		if done%ProgressLogEvery == 0 || done == int64(len(docURLs)) {
			log.Printf("Fetched %d/%d markdown documents", done, len(docURLs))
		}
	}

	recordFailure := func(jobURL string, failure manifest.FetchFailure) {
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
						relativePath, err = links.RelativePathForURL(job.url, layout)
						if err != nil {
							recordFailure(job.url, manifest.FetchFailure{URL: job.url, Error: fmt.Sprintf("map URL: %v", err)})
							continue
						}
					}

					if err := urlPolicy.Validate(job.url); err != nil {
						failure := manifest.FetchFailure{URL: job.url, Error: err.Error()}
						preserved, preservedErr := PreservePreviousDocument(snapshotRoot, job.url, relativePath, previous)
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

					result, err := FetchDocument(ctx, client, urlPolicy, spoolDir, snapshotRoot, job.url, relativePath, previous)
					if err != nil {
						failure := BuildFetchFailure(diagnosticsDir, job.url, relativePath, err)
						preserved, preservedErr := PreservePreviousDocument(snapshotRoot, job.url, relativePath, previous)
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

	finalResults := make([]Result, 0, len(docURLs))
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

func PreservePreviousDocument(snapshotRoot string, rawURL string, relativePath string, previous manifest.Entry) (Result, error) {
	if previous.Path == "" {
		return Result{}, errors.New("no previous snapshot entry")
	}

	previousPath := previous.Path
	if previousPath == "" {
		previousPath = filepath.ToSlash(relativePath)
	}

	localPath, sha256Value, bytesCount, err := SummarizeExistingFile(snapshotRoot, previousPath)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCachedSummary(previousPath, sha256Value, bytesCount, previous); err != nil {
		return Result{}, err
	}

	return Result{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}, nil
}

func FetchDocument(
	ctx context.Context,
	client *http.Client,
	urlPolicy *policy.URLPolicy,
	spoolDir string,
	snapshotRoot string,
	rawURL string,
	relativePath string,
	previous manifest.Entry,
) (Result, error) {
	if err := urlPolicy.Validate(rawURL); err != nil {
		return Result{}, err
	}

	requireMarkdown, err := links.IsMarkdownURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	validators := Validators{
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}

	attempts := 1
	if requireMarkdown {
		attempts = MarkdownFetchAttempts
	}

	for attempt := 0; attempt < attempts; attempt++ {
		response, err := FetchURL(ctx, client, rawURL, spoolDir, validators)
		if err != nil {
			return Result{}, err
		}

		if response.NotModified {
			cachedResult, cacheErr := LoadCachedDocument(snapshotRoot, rawURL, relativePath, previous, response)
			if cacheErr == nil {
				return cachedResult, nil
			}

			response, err = FetchURL(ctx, client, rawURL, spoolDir, Validators{})
			if err != nil {
				return Result{}, fmt.Errorf("%v (after cache-miss refetch failed: %w)", cacheErr, err)
			}
		}

		if requireMarkdown {
			if err := EnsureMarkdownResponse(response.Status, response.ContentType, response.Headers, response.Sniff, response.BodyPath); err != nil {
				var unexpected *UnexpectedContentError
				if errors.As(err, &unexpected) && attempt+1 < attempts {
					CleanupSpoolFile(response.BodyPath)
					if sleepErr := RetrySleepWithJitter(ctx, attempt); sleepErr != nil {
						return Result{}, sleepErr
					}
					continue
				}
				return Result{}, err
			}
		}

		return Result{
			URL:            rawURL,
			RelativePath:   relativePath,
			LocalPath:      response.BodyPath,
			SHA256:         response.SHA256,
			Bytes:          response.Bytes,
			LastModifiedAt: response.LastModifiedAt,
			ETag:           response.ETag,
		}, nil
	}

	return Result{}, fmt.Errorf("failed to fetch %s", rawURL)
}

func LoadCachedDocument(snapshotRoot string, rawURL string, relativePath string, previous manifest.Entry, response HTTPResponse) (Result, error) {
	localPath, sha256Value, bytesCount, err := SummarizeExistingFile(snapshotRoot, filepath.ToSlash(relativePath))
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCachedSummary(filepath.ToSlash(relativePath), sha256Value, bytesCount, previous); err != nil {
		return Result{}, err
	}

	return Result{
		URL:            rawURL,
		RelativePath:   relativePath,
		LocalPath:      localPath,
		SHA256:         sha256Value,
		Bytes:          bytesCount,
		LastModifiedAt: CoalesceValidator(response.LastModifiedAt, previous.LastModifiedAt),
		ETag:           CoalesceValidator(response.ETag, previous.ETag),
	}, nil
}

func ValidateCachedSummary(relativePath string, sha256Value string, bytesCount int64, previous manifest.Entry) error {
	if previous.SHA256 != "" && previous.SHA256 != sha256Value {
		return fmt.Errorf("cached file %s does not match previous manifest hash", relativePath)
	}
	if previous.Bytes > 0 && previous.Bytes != bytesCount {
		return fmt.Errorf("cached file %s size does not match previous manifest", relativePath)
	}
	return nil
}

func FetchURL(ctx context.Context, client *http.Client, rawURL string, spoolDir string, validators Validators) (response HTTPResponse, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return HTTPResponse{}, err
	}

	req.Header.Set("User-Agent", buildUserAgent())
	req.Header.Set("Accept", "text/plain, text/markdown, text/*;q=0.9, */*;q=0.1")
	if ifNoneMatch := NormalizeETag(validators.ETag); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if ifModifiedSince := IfModifiedSinceHeader(validators.LastModifiedAt); ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := client.Do(req)
	if err != nil {
		return HTTPResponse{}, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()

	response = HTTPResponse{
		Status:         resp.Status,
		ContentType:    strings.TrimSpace(resp.Header.Get("Content-Type")),
		Headers:        copyHeader(resp.Header),
		LastModifiedAt: NormalizeLastModified(resp.Header.Get("Last-Modified")),
		ETag:           NormalizeETag(resp.Header.Get("ETag")),
		NotModified:    resp.StatusCode == http.StatusNotModified,
	}

	if resp.StatusCode == http.StatusNotModified {
		response.LastModifiedAt = CoalesceValidator(response.LastModifiedAt, validators.LastModifiedAt)
		response.ETag = CoalesceValidator(response.ETag, validators.ETag)
		return response, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HTTPResponse{}, fmt.Errorf("unexpected HTTP %s", resp.Status)
	}

	spoolFile, err := os.CreateTemp(spoolDir, "fetch-*")
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("create spool file: %w", err)
	}

	cleanupOnError := true
	defer func() {
		_ = spoolFile.Close()
		if cleanupOnError {
			_ = os.Remove(spoolFile.Name())
		}
	}()

	hasher := sha256.New()
	capture := &PrefixCaptureWriter{Limit: HTMLSniffBytes}

	written, err := io.Copy(io.MultiWriter(spoolFile, hasher, capture), resp.Body)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("stream response body: %w", err)
	}

	response.BodyPath = spoolFile.Name()
	response.Sniff = append([]byte(nil), capture.Buf...)
	response.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	response.Bytes = written

	cleanupOnError = false
	return response, nil
}

func EnsureMarkdownResponse(status string, contentType string, headers map[string][]string, sniff []byte, bodyPath string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return &UnexpectedContentError{
			Message:     fmt.Sprintf("expected markdown response but received %s", mediaType),
			Status:      status,
			ContentType: contentType,
			Headers:     headers,
			Sniff:       append([]byte(nil), sniff...),
			BodyPath:    bodyPath,
		}
	}

	if LooksLikeHTMLDocument(sniff) {
		return &UnexpectedContentError{
			Message:     "expected markdown response but received HTML document",
			Status:      status,
			ContentType: contentType,
			Headers:     headers,
			Sniff:       append([]byte(nil), sniff...),
			BodyPath:    bodyPath,
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

func LooksLikeHTMLDocument(body []byte) bool {
	trimmed := NormalizeHTMLSniff(body)
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<head") ||
		strings.HasPrefix(lower, "<body")
}

func NormalizeHTMLSniff(body []byte) string {
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

func IfModifiedSinceHeader(lastModifiedAt string) string {
	if lastModifiedAt == "" {
		return ""
	}

	parsed, err := time.Parse(time.RFC3339, lastModifiedAt)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(http.TimeFormat)
}

func NormalizeLastModified(value string) string {
	if value == "" {
		return ""
	}

	parsed, err := http.ParseTime(value)
	if err != nil {
		return ""
	}

	return parsed.UTC().Format(time.RFC3339)
}

func NormalizeETag(value string) string {
	return strings.TrimSpace(value)
}

func CoalesceValidator(current string, previous string) string {
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

func HashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func SummarizeExistingFile(root string, relativePath string) (localPath string, sha256Value string, bytesCount int64, err error) {
	localPath, err = SafeJoin(root, relativePath)
	if err != nil {
		return "", "", 0, err
	}

	// #nosec G304 -- localPath is anchored to snapshotRoot via SafeJoin.
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

func SafeJoin(root string, relativePath string) (string, error) {
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

func CleanupSpoolFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

func BuildFetchFailure(diagnosticsDir string, rawURL string, relativePath string, err error) manifest.FetchFailure {
	failure := manifest.FetchFailure{URL: rawURL, Error: err.Error()}

	var unexpected *UnexpectedContentError
	if errors.As(err, &unexpected) {
		diagnosticPath, diagnosticErr := WriteUnexpectedContentDiagnostic(diagnosticsDir, rawURL, relativePath, unexpected)
		if diagnosticErr != nil {
			failure.Error = fmt.Sprintf("%s (failed to write diagnostic: %v)", failure.Error, diagnosticErr)
		} else {
			failure.DiagnosticPath = diagnosticPath
		}
	}

	return failure
}

func WriteUnexpectedContentDiagnostic(diagnosticsDir string, rawURL string, relativePath string, unexpected *UnexpectedContentError) (string, error) {
	if diagnosticsDir == "" {
		return "", nil
	}

	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(unexpected.ContentType, ";")[0]))
	bodyExtension := ".txt"
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" || LooksLikeHTMLDocument(unexpected.Sniff) {
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
	if err := copyFile(unexpected.BodyPath, bodyPath); err != nil {
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
		Error:        unexpected.Message,
		Status:       unexpected.Status,
		ContentType:  unexpected.ContentType,
		Headers:      unexpected.Headers,
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

func copyFile(sourcePath string, targetPath string) (err error) {
	// #nosec G304 -- sourcePath is a local spool or cached snapshot path produced by the crawler.
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}

	// #nosec G304 -- targetPath is a diagnostic path rooted under a temp directory controlled by the crawler.
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
	// #nosec G302 -- diagnostics copies are intended to remain world-readable in uploaded artifacts.
	if err := os.Chmod(targetPath, 0o644); err != nil {
		return err
	}

	success = true
	return nil
}
