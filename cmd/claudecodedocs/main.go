// Package main implements the claudecodedocs CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"claudecodedocs/internal/app"
	"claudecodedocs/internal/links"
)

const (
	defaultTimeout = 30 * time.Second
	defaultWorkers = 8
	defaultLayout  = links.LayoutNested
)

type cli struct {
	run func(context.Context, app.Config) error
}

func newCLI() *cli { return &cli{run: app.Run} }

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg := parseFlags()
	cfg.Logger = logger
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := newCLI()
	if err := c.run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags() app.Config {
	var cfg app.Config

	flag.StringVar(&cfg.SourceURL, "source", "", "URL of the llms.txt index")
	flag.StringVar(&cfg.OutputDir, "out", "", "directory where the generated snapshot is written")
	flag.StringVar(&cfg.Layout, "layout", defaultLayout, "output layout: root or nested")
	flag.StringVar(&cfg.PreviousManifestPath, "previous-manifest", "", "path to a previously released manifest.json")
	flag.StringVar(&cfg.ManifestOut, "manifest-out", "", "path where the fresh manifest.json is written")
	flag.StringVar(&cfg.DiagnosticsDir, "diagnostics-dir", "", "directory where fetch diagnostics are written on failure")
	flag.StringVar(&cfg.AllowedHostsCSV, "allowed-hosts", "", "comma-separated additional hosts allowed for document fetches")
	flag.DurationVar(&cfg.Timeout, "timeout", defaultTimeout, "HTTP timeout per request")
	flag.IntVar(&cfg.Concurrency, "concurrency", defaultWorkers, "maximum number of concurrent fetches")
	flag.Float64Var(&cfg.RateLimit, "rate-limit", 0, "maximum requests per second (0 = unlimited)")
	flag.Parse()

	if cfg.SourceURL == "" {
		fmt.Fprintln(os.Stderr, "missing required -source")
		os.Exit(1)
	}
	if cfg.OutputDir == "" {
		fmt.Fprintln(os.Stderr, "missing required -out")
		os.Exit(1)
	}
	if cfg.ManifestOut == "" {
		fmt.Fprintln(os.Stderr, "missing required -manifest-out")
		os.Exit(1)
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Layout != links.LayoutRoot && cfg.Layout != links.LayoutNested {
		fmt.Fprintf(os.Stderr, "invalid -layout %q (expected %q or %q)\n", cfg.Layout, links.LayoutRoot, links.LayoutNested)
		os.Exit(1)
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve current working directory: %v\n", err)
		os.Exit(1)
	}
	cfg.SnapshotRoot = wd

	return cfg
}
