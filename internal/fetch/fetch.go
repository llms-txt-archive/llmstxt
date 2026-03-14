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

	"claudecodedocs/internal/links"
	"claudecodedocs/internal/manifest"
	"claudecodedocs/internal/policy"

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
var ErrNoPreviousEntry = errors.New("no previous snapshot entry")

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

// Options configures a batch document fetch.
type Options struct {
	Client            *http.Client
	URLPolicy         *policy.URLPolicy
	Layout            string
	DiagnosticsDir    string
	SpoolDir          string
	SnapshotRoot      string
	Concurrency       int
	PreviousDocuments map[string]manifest.Entry
	// RateLimiter controls the rate of outbound HTTP requests. Nil means no limit.
	RateLimiter *rate.Limiter
	// RetrySleep overrides the retry sleep function. Defaults to sleepWithJitter if nil.
	RetrySleep func(context.Context, int) error
	Logger     *slog.Logger
}

func (o *Options) retrySleep() func(context.Context, int) error {
	if o.RetrySleep != nil {
		return o.RetrySleep
	}
	return sleepWithJitter
}

func (o *Options) logger() *slog.Logger {
	if o != nil && o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
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

	recordFailure := func(jobURL string, failure manifest.FetchFailure) {
		opts.logger().Warn("fetch failure", "url", jobURL, "error", failure.Error)
		mu.Lock()
		failures = append(failures, failure)
		mu.Unlock()
		recordCompletion()
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
				case job, ok := <-jobs:
					if !ok {
						return
					}

					previous := opts.PreviousDocuments[job.url]
					relativePath := filepath.FromSlash(previous.Path)
					if relativePath == "" {
						var err error
						relativePath, err = links.RelativePath(job.url, opts.Layout)
						if err != nil {
							recordFailure(job.url, manifest.FetchFailure{URL: job.url, Error: fmt.Sprintf("map URL: %v", err)})
							continue
						}
					}

					if opts.RateLimiter != nil {
						if err := opts.RateLimiter.Wait(ctx); err != nil {
							return
						}
					}

					if err := opts.URLPolicy.Validate(job.url); err != nil {
						failure := manifest.FetchFailure{URL: job.url, Error: err.Error()}
						preserved, preservedErr := PreservePreviousDocument(opts.SnapshotRoot, job.url, relativePath, previous)
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

					result, err := Document(ctx, opts.Client, opts.URLPolicy, opts.SpoolDir, opts.SnapshotRoot, job.url, relativePath, previous, opts.retrySleep())
					if err != nil {
						failure := BuildFetchFailure(opts.DiagnosticsDir, job.url, relativePath, err)
						preserved, preservedErr := PreservePreviousDocument(opts.SnapshotRoot, job.url, relativePath, previous)
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

// Document downloads a single document URL, using conditional requests and retries as appropriate.
// If retrySleep is nil, the default sleepWithJitter is used.
func Document(
	ctx context.Context,
	client *http.Client,
	urlPolicy *policy.URLPolicy,
	spoolDir string,
	snapshotRoot string,
	rawURL string,
	relativePath string,
	previous manifest.Entry,
	retrySleep func(context.Context, int) error,
) (Result, error) {
	if retrySleep == nil {
		retrySleep = sleepWithJitter
	}
	if err := urlPolicy.Validate(rawURL); err != nil {
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
		response, err := URL(ctx, client, rawURL, spoolDir, validators)
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
			cachedResult, cacheErr := LoadCachedDocument(snapshotRoot, rawURL, relativePath, previous, response)
			if cacheErr == nil {
				return cachedResult, nil
			}

			response, err = URL(ctx, client, rawURL, spoolDir, Validators{})
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

	return Result{}, fmt.Errorf("failed to fetch %s", rawURL)
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
