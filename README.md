# Claude Code Docs Tracker

This repo snapshots the Markdown documentation listed in [Claude Code's `llms.txt`](https://code.claude.com/docs/llms.txt) so a committed baseline can be monitored for upstream changes.

The crawler is intentionally small:

- written in Go
- standard library only
- saves raw Markdown for every URL listed in `llms.txt`
- stores per-page `Last-Modified` metadata in `snapshot/manifest.json`
- uses `If-Modified-Since` on later runs when the upstream server supports it
- writes a deterministic `snapshot/manifest.json` so unchanged runs do not create noisy commits
- designed to run locally or from GitHub Actions

## Repository layout

```text
.
├── .github/workflows/sync.yml
├── cmd/claudecodedocs/main.go
├── snapshot/
│   ├── manifest.json
│   ├── pages/code.claude.com/docs/en/*.md
│   └── source/llms.txt
└── README.md
```

## Run locally

```bash
go run ./cmd/claudecodedocs
```

Useful flags:

```bash
go run ./cmd/claudecodedocs \
  -source https://code.claude.com/docs/llms.txt \
  -out snapshot \
  -concurrency 8 \
  -timeout 30s
```

## How the history works

Each run:

1. downloads `llms.txt`
2. extracts every linked URL
3. fetches each page
4. mirrors the page content into `snapshot/pages/...`
5. rewrites `snapshot/manifest.json`

If nothing in `snapshot/` changes, the GitHub Actions workflow exits quietly. If something changes, it uploads diff artifacts and can notify Discord without committing anything back to the repo.

## GitHub setup

1. Create a GitHub repository from this directory or push this directory to an existing repo.
2. Make sure Actions are enabled for the repo.
3. Commit the initial `snapshot/` so the workflow has a baseline to compare against.
4. Add a repository secret named `DISCORD_WEBHOOK_URL` when you're ready to enable notifications.
5. Keep the default workflow schedule or edit `.github/workflows/sync.yml` if you want a different cadence.

The included workflow:

- runs every hour and on manual dispatch
- executes `go test ./...`
- runs the crawler
- ignores metadata-only changes when `snapshot/manifest.json` is the only modified file
- uploads diff artifacts when crawled source files actually change
- posts a Discord message when a new diff hash is detected and `DISCORD_WEBHOOK_URL` is configured

## Notes

- The workflow only needs read access because it no longer commits back to the repo.
- The manifest is content-based on purpose, so the schedule alone does not generate commits.
- `Last-Modified` data is only present when the upstream server sends that header.
- Discord alert deduplication is best-effort and keyed off the current `snapshot/manifest.json` hash.
- Because the docs are stored as raw Markdown, normal git diff tools are enough to inspect changes over time.
