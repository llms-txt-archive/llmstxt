package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
	"time"
)

type config struct {
	templatePath  string
	outputPath    string
	title         string
	siteName      string
	siteURL       string
	sourceURL     string
	scheduleLabel string
	documentCount int
	skippedCount  int
	releasesPath  string
}

type releaseInput struct {
	Name        string `json:"name"`
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

type releaseView struct {
	Name           string
	TagName        string
	PublishedLabel string
	HTMLURL        string
}

type templateData struct {
	Title         string
	SiteName      string
	SiteURL       string
	SourceURL     string
	ScheduleLabel string
	DocumentCount int
	SkippedCount  int
	Releases      []releaseView
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.templatePath, "template", "", "path to the README template")
	flag.StringVar(&cfg.outputPath, "out", "", "path to the rendered README.md")
	flag.StringVar(&cfg.title, "title", "", "README title")
	flag.StringVar(&cfg.siteName, "site-name", "", "human-readable site name")
	flag.StringVar(&cfg.siteURL, "site-url", "", "site URL")
	flag.StringVar(&cfg.sourceURL, "source-url", "", "source llms.txt URL")
	flag.StringVar(&cfg.scheduleLabel, "schedule-label", "", "human-readable sync schedule")
	flag.IntVar(&cfg.documentCount, "document-count", 0, "count of tracked markdown documents")
	flag.IntVar(&cfg.skippedCount, "skipped-count", 0, "count of skipped non-markdown URLs")
	flag.StringVar(&cfg.releasesPath, "releases-json", "", "path to release metadata JSON")
	flag.Parse()

	required := map[string]string{
		"-template":       cfg.templatePath,
		"-out":            cfg.outputPath,
		"-title":          cfg.title,
		"-site-name":      cfg.siteName,
		"-site-url":       cfg.siteURL,
		"-source-url":     cfg.sourceURL,
		"-schedule-label": cfg.scheduleLabel,
		"-releases-json":  cfg.releasesPath,
	}

	for flagName, value := range required {
		if value == "" {
			fmt.Fprintf(os.Stderr, "missing required %s\n", flagName)
			os.Exit(1)
		}
	}

	return cfg
}

func run(cfg config) error {
	releases, err := loadReleases(cfg.releasesPath)
	if err != nil {
		return err
	}

	tmpl, err := template.ParseFiles(cfg.templatePath)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	data := templateData{
		Title:         cfg.title,
		SiteName:      cfg.siteName,
		SiteURL:       cfg.siteURL,
		SourceURL:     cfg.sourceURL,
		ScheduleLabel: cfg.scheduleLabel,
		DocumentCount: cfg.documentCount,
		SkippedCount:  cfg.skippedCount,
		Releases:      releases,
	}

	if err := os.MkdirAll(filepath.Dir(cfg.outputPath), 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	outputFile, err := os.Create(cfg.outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}

	if err := tmpl.Execute(outputFile, data); err != nil {
		_ = outputFile.Close()
		return fmt.Errorf("render template: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return fmt.Errorf("close output file: %w", err)
	}

	return nil
}

func loadReleases(releasesPath string) ([]releaseView, error) {
	// #nosec G304 -- releasesPath is a local CLI input to a checked-out JSON file.
	body, err := os.ReadFile(releasesPath)
	if err != nil {
		return nil, fmt.Errorf("read releases json: %w", err)
	}

	var inputs []releaseInput
	if err := json.Unmarshal(body, &inputs); err != nil {
		return nil, fmt.Errorf("parse releases json: %w", err)
	}

	releases := make([]releaseView, 0, len(inputs))
	for _, input := range inputs {
		releases = append(releases, releaseView{
			Name:           input.Name,
			TagName:        input.TagName,
			PublishedLabel: formatPublishedLabel(input.PublishedAt),
			HTMLURL:        input.HTMLURL,
		})
	}

	return releases, nil
}

func formatPublishedLabel(value string) string {
	if value == "" {
		return "unknown"
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}

	return parsed.UTC().Format("2006-01-02 15:04 UTC")
}
