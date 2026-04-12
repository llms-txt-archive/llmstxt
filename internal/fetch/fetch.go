// Package fetch downloads and validates documents referenced by an llms.txt index.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llms-txt-archive/llmstxt/internal/links"
	"github.com/llms-txt-archive/llmstxt/internal/logutil"
	"github.com/llms-txt-archive/llmstxt/internal/manifest"
	"github.com/llms-txt-archive/llmstxt/internal/policy"

	"golang.org/x/time/rate"
)

const (
	// MarkdownFetchAttempts retries markdown URLs up to 3 times to handle
	// transient CDN responses that initially serve HTML error pages.
	MarkdownFetchAttempts = 3
	// TransientRetryAttempts retries non-markdown URLs up to 2 times to
	// handle transient HTTP errors (5xx, 429).
	TransientRetryAttempts = 2
	// ProgressLogEvery logs a progress line every 25 completed fetches
	// to avoid flooding logs during large syncs.
	ProgressLogEvery = 25
)

// ErrNoPreviousEntry is returned when no previous manifest entry exists for a document.
var ErrNoPreviousEntry = errors.New("no previous archive entry")

// Result holds the outcome of fetching a single URL.
type Result struct {
	URL            string
	RelativePath   string
	LocalPath      string
	SHA256         string
	Bytes          int64
	LastModifiedAt string
	ETag           string
}

// Validators holds conditional-request values for HTTP caching headers.
type Validators struct {
	LastModifiedAt string
	ETag           string
}

// ClientConfig holds the shared HTTP client configuration used by both single and batch fetches.
type ClientConfig struct {
	Client      *http.Client
	URLPolicy   *policy.URLPolicy
	SpoolDir    string
	ArchiveRoot string
}

// Options configures a batch document fetch.
type Options struct {
	ClientConfig
	Layout         string
	DiagnosticsDir string
	Concurrency    int
	PreviousDocs   map[string]manifest.Entry
	// RateLimiter controls the rate of outbound HTTP requests. Nil means no limit.
	RateLimiter *rate.Limiter
	// RetrySleep overrides the retry sleep function. Defaults to sleepWithJitter if nil.
	RetrySleep func(context.Context, int) error
	Logger     *slog.Logger
}

func (o *Options) logger() *slog.Logger {
	if o != nil {
		return logutil.Default(o.Logger)
	}
	return slog.Default()
}

// jobResult holds the outcome of processing a single document fetch job.
type jobResult struct {
	result  Result
	failure *manifest.FetchFailure
	ok      bool
}

// processJob fetches a single document, falling back to a preserved previous copy on failure.
func processJob(ctx context.Context, rawURL string, opts Options) jobResult {
	var previous manifest.Entry
	if opts.PreviousDocs != nil {
		previous = opts.PreviousDocs[rawURL]
	}
	relativePath := filepath.FromSlash(previous.Path)
	if relativePath == "" {
		var err error
		relativePath, err = links.RelativePath(rawURL, opts.Layout)
		if err != nil {
			return jobResult{failure: &manifest.FetchFailure{URL: rawURL, Error: fmt.Sprintf("map URL: %v", err)}}
		}
	}

	if opts.RateLimiter != nil {
		if err := opts.RateLimiter.Wait(ctx); err != nil {
			return jobResult{}
		}
	}

	fetchErr := opts.URLPolicy.Validate(rawURL)
	if fetchErr == nil {
		result, err := Document(ctx, rawURL, relativePath, previous, DocumentConfig{
			ClientConfig: opts.ClientConfig,
			RetrySleep:   opts.RetrySleep,
		})
		if err == nil {
			return jobResult{result: result, ok: true}
		}
		fetchErr = err
	}

	failure := BuildFetchFailure(opts.DiagnosticsDir, rawURL, relativePath, fetchErr)
	preserved, preservedErr := PreservePreviousDocument(opts.ArchiveRoot, rawURL, relativePath, previous)
	if preservedErr == nil {
		failure.PreservedExisting = true
		return jobResult{result: preserved, failure: &failure, ok: true}
	}
	if previous.Path != "" {
		failure.Error = fmt.Sprintf("%s (failed to preserve previous copy: %v)", failure.Error, preservedErr)
	}
	return jobResult{failure: &failure}
}

// Documents downloads all document URLs concurrently and returns successful results and failures.
func Documents(ctx context.Context, docURLs []string, opts Options) ([]Result, []manifest.FetchFailure) {
	type job struct {
		index int
		url   string
	}

	results := make([]Result, len(docURLs))
	succeeded := make([]bool, len(docURLs))
	var failures []manifest.FetchFailure

	var (
		mu        sync.Mutex
		wg        sync.WaitGroup
		completed atomic.Int64
	)

	recordCompletion := func() {
		done := completed.Add(1)
		if done%ProgressLogEvery == 0 || done == int64(len(docURLs)) {
			opts.logger().Info("fetch progress", "done", done, "total", len(docURLs))
		}
	}

	jobs := make(chan job)
	for worker := 0; worker < opts.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}

					jr := processJob(ctx, j.url, opts)

					mu.Lock()
					if jr.ok {
						results[j.index] = jr.result
						succeeded[j.index] = true
					}
					if jr.failure != nil {
						opts.logger().Warn("fetch failure", "url", j.url, "error", jr.failure.Error)
						failures = append(failures, *jr.failure)
					}
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

// DocumentConfig holds the shared configuration for fetching a single document.
type DocumentConfig struct {
	ClientConfig
	// RetrySleep overrides the retry sleep function. Defaults to sleepWithJitter if nil.
	RetrySleep func(context.Context, int) error
}

func (c *DocumentConfig) retrySleep() func(context.Context, int) error {
	if c != nil && c.RetrySleep != nil {
		return c.RetrySleep
	}
	return sleepWithJitter
}

// Document downloads a single document URL, using conditional requests and retries as appropriate.
func Document(ctx context.Context, rawURL string, relativePath string, previous manifest.Entry, cfg DocumentConfig) (Result, error) {
	retrySleep := cfg.retrySleep()
	if err := cfg.URLPolicy.Validate(rawURL); err != nil {
		return Result{}, err
	}

	requireMarkdown, err := links.IsMarkdown(rawURL)
	if err != nil {
		return Result{}, err
	}
	validators := Validators{
		LastModifiedAt: previous.LastModifiedAt,
		ETag:           previous.ETag,
	}

	attempts := TransientRetryAttempts
	if requireMarkdown {
		attempts = MarkdownFetchAttempts
	}

	for attempt := 0; attempt < attempts; attempt++ {
		response, err := Get(ctx, cfg.Client, rawURL, cfg.SpoolDir, validators)
		if err != nil {
			var transient *TransientHTTPError
			if errors.As(err, &transient) && attempt+1 < attempts {
				if sleepErr := retrySleep(ctx, attempt); sleepErr != nil {
					return Result{}, sleepErr
				}
				continue
			}
			return Result{}, err
		}

		if response.NotModified {
			cachedResult, cacheErr := LoadCachedDocument(cfg.ArchiveRoot, rawURL, relativePath, previous, response)
			if cacheErr == nil {
				return cachedResult, nil
			}

			response, err = Get(ctx, cfg.Client, rawURL, cfg.SpoolDir, Validators{})
			if err != nil {
				return Result{}, fmt.Errorf("cache validation failed: %w; refetch also failed: %w", cacheErr, err)
			}
		}

		if requireMarkdown {
			if err := EnsureMarkdownResponse(response.Status, response.ContentType, response.Headers, response.Sniff, response.BodyPath); err != nil {
				var unexpected *UnexpectedContentError
				if errors.As(err, &unexpected) && attempt+1 < attempts {
					CleanupSpoolFile(response.BodyPath)
					if sleepErr := retrySleep(ctx, attempt); sleepErr != nil {
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

	// All retry paths return within the loop; this satisfies the compiler.
	return Result{}, fmt.Errorf("failed to fetch %s after %d attempts", rawURL, attempts)
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
