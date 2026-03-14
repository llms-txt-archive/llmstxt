# llmstxt

Tooling for archiving documentation exposed via [`llms.txt`](https://llmstxt.org/) and tracking how it changes over time. Any site that publishes an `llms.txt` index can be archived.

## What this repo contains

| Component | Path | Purpose |
|-----------|------|---------|
| Crawler | `cmd/claudecodedocs/` | Fetches an `llms.txt` index and all linked `.md` documents |
| README renderer | `cmd/snapshotreadme/` | Generates a README for archive repos from a Go template |
| Reusable workflow | `.github/workflows/snapshot-sync.yml` | End-to-end sync: crawl, diff, commit, release |
| Codex integration | `.github/codex/`, `.github/scripts/` | AI-generated release notes with hardened validation |
| CI | `.github/workflows/ci.yml` | Tests, linting, vulnerability scanning |

## Architecture

```
llmstxt (this repo)                        Archive repo (consumer)
========================                   ==========================================
cmd/claudecodedocs/  (crawler binary)      .github/workflows/sync.yml (caller)
cmd/snapshotreadme/  (readme binary)       *.md files at root (generated)
internal/            (Go packages)         README.md (generated)
.github/workflows/snapshot-sync.yml  <---  calls this reusable workflow
templates/           (readme template)     releases → manifest.json (asset, NOT tracked)
```

Archive repos are thin: a caller workflow, raw `.md` files at root, and a generated `README.md`. No fetched documents or manifests live in this tool repo.

### How a sync runs

1. Archive repo's `sync.yml` calls `snapshot-sync.yml` on a schedule (e.g. hourly)
2. Workflow checks out both the archive repo and this tool repo
3. Downloads the previous `manifest.json` from the latest release (for conditional requests)
4. Runs the crawler: fetches `llms.txt`, discovers linked docs (including nested indexes via BFS), downloads all `.md` files concurrently
5. Syncs generated files into the archive repo root via `rsync`
6. If content changed: commits, creates a tagged release with `manifest.json` as an asset
7. For non-initial releases, Codex generates the commit message and release notes

### Crawler features

- Parses both markdown-link and plain URL-per-line `llms.txt` formats
- BFS discovery of nested `llms.txt` indexes (capped at 50 to prevent runaway crawling)
- Concurrent fetching with configurable worker count and optional rate limiting (`-rate-limit`)
- Conditional requests via `If-None-Match` / `If-Modified-Since` using the previous manifest
- Retries transient HTTP errors (5xx, 429) with exponential backoff and `Retry-After` support
- Response body size capped at 256 MiB per document
- HTTPS-only with SSRF protection (blocks private/loopback IPs, validates DNS resolution)
- Content validation: rejects `.md` URLs that return HTML (CDN error pages, login redirects)
- Atomic output staging with crash recovery journaling
- Preserves the previous snapshot copy when individual documents fail to fetch
- Structured logging via `log/slog`
- Two output layouts: `root` (flat) and `nested` (by host)

## Local usage

```bash
# Build
make build

# Crawl a site
go run ./cmd/claudecodedocs \
  -source https://example.com/llms.txt \
  -out /tmp/snapshot \
  -layout root \
  -manifest-out /tmp/manifest.json

# With rate limiting and cross-host support
go run ./cmd/claudecodedocs \
  -source https://example.com/llms.txt \
  -allowed-hosts docs.examplecdn.com \
  -rate-limit 5 \
  -out /tmp/snapshot \
  -layout root \
  -manifest-out /tmp/manifest.json

# Render a README
go run ./cmd/snapshotreadme \
  -template templates/snapshot-readme.md.tmpl \
  -out /tmp/README.md \
  -title "My Docs Archive" \
  -site-name "Example Docs" \
  -site-url "https://example.com" \
  -source-url "https://example.com/llms.txt" \
  -schedule-label "Hourly" \
  -document-count 42 \
  -skipped-count 3
```

## Development

```bash
make check    # vet + test (race) + lint + govulncheck
make test     # tests only, with race detector
make build    # build both binaries to bin/
```

## Setting up a new archive repo

1. Create a new repo (e.g. `my-docs-archive`)
2. Add a caller workflow at `.github/workflows/sync.yml`:

```yaml
name: Sync
on:
  schedule:
    - cron: "42 * * * *"   # hourly
  workflow_dispatch:

jobs:
  sync:
    uses: f-pisani/llmstxt/.github/workflows/snapshot-sync.yml@main
    with:
      source_url: "https://example.com/llms.txt"
      site_name: "Example Docs"
      site_url: "https://example.com"
      repo_title: "Example Docs Archive"
      schedule_label: "Hourly at :42 UTC"
      tool_ref: main
    secrets:
      OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
```

3. Set the `OPENAI_API_KEY` secret (required for AI-generated release notes after the initial sync)
4. Optionally set `CODEX_MODEL` and `CODEX_EFFORT` repository variables to override Codex defaults

> **Versioning:** We recommend pinning `tool_ref` to a tagged release (e.g. `v1.0.0`) for stability. Tagged releases are planned.

## Invariants for agents working on this codebase

If you're an AI agent modifying this repo, these rules are critical:

- **`manifest.json` is a release asset.** It is never git-tracked in archive repos. It's uploaded on release creation and downloaded from the previous release for incremental syncs.
- **The archive repo contract is sacred.** `snapshot-sync.yml` outputs raw `.md` at root + generated `README.md`. Changing this structure breaks all consumer repos.
- **Codex validation is security-hardened.** The validator (`validate_codex_release.py`) rejects prompt injection, placeholder text, and instruction-following patterns. Do not weaken these checks.
- **This repo contains no fetched documents.** Generated output (markdown files, manifests) belongs in archive repos. The `/snapshot/` directory is gitignored.
- **The goal is tracking documentation changes over time.** Every sync is a release. The release history IS the changelog of how documentation evolved.

## Future direction

This repo will move to the [`llms-txt-archive`](https://github.com/llms-txt-archive) GitHub org and may evolve into a monorepo hosting additional tools for the `llms.txt` ecosystem.
