package main

import (
	"context"
	"flag"
	"log"
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

var runApp = app.Run

func main() {
	log.SetFlags(0)

	cfg := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runApp(ctx, cfg); err != nil {
		log.Fatal(err)
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
	flag.Parse()

	if cfg.SourceURL == "" {
		log.Fatal("missing required -source")
	}
	if cfg.OutputDir == "" {
		log.Fatal("missing required -out")
	}
	if cfg.ManifestOut == "" {
		log.Fatal("missing required -manifest-out")
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Layout != links.LayoutRoot && cfg.Layout != links.LayoutNested {
		log.Fatalf("invalid -layout %q (expected %q or %q)", cfg.Layout, links.LayoutRoot, links.LayoutNested)
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve current working directory: %v", err)
	}
	cfg.SnapshotRoot = wd

	return cfg
}
