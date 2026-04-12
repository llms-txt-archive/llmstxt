# Agent Guidelines

## Project goal

Archive documentation exposed via `llms.txt` and track how it changes over time. Every sync is a release. The release history IS the changelog of documentation evolution.

## Repository layout

This is the **tool repo**. It builds the crawler and workflow that archive repos consume. It contains zero fetched documents — generated output lives exclusively in archive repos.

- `cmd/crawler/` — fetches an `llms.txt` index and all linked `.md` documents
- `cmd/readmegen/` — renders README for archive repos from a Go template
- `internal/` — shared Go packages (app, fetch, fileutil, links, manifest, policy, stage)
- `.github/workflows/archive-sync.yml` — reusable workflow: crawl, diff, commit, release
- `.github/codex/` — Codex prompt and schema for AI-generated release notes
- `.github/scripts/` — Python validators, emitters, and bash helpers for Codex pipeline
- `templates/` — Go template for archive repo READMEs

## Rules

- **`manifest.json` is a release asset.** Never git-track it. It's uploaded on release creation and downloaded from the previous release for conditional requests.
- **The archive repo contract is sacred.** `archive-sync.yml` outputs raw `.md` at root + generated `README.md`. Changing this structure breaks all consumer repos.
- **Codex validation is security-hardened.** The validator (`validate_codex_release.py`) rejects prompt injection, placeholder text, and instruction-following patterns. Do not weaken these checks.
- **No fetched documents in this repo.** The `/archive/` directory is gitignored. All generated output belongs in archive repos.

## Go module

The module is `github.com/llms-txt-archive/llmstxt`. Import paths use `github.com/llms-txt-archive/llmstxt/internal/...`.

## Testing

```bash
make check    # vet + test (race) + lint + govulncheck
make test     # tests only, with race detector
```

All tests must pass with the race detector enabled. Unit tests live alongside source in `internal/` packages. Integration tests are in `cmd/crawler/main_test.go`.

## CI

CI runs on every push and PR: `go mod verify`, race-enabled tests, `go vet`, `golangci-lint`, `govulncheck`, `nilaway`, and Python helper tests.
