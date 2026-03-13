# Claude Docs Snapshot Tooling

This repository is the shared tooling repo for archive-style documentation mirrors driven by `llms.txt`.

It contains:

- the generic crawler in `cmd/claudecodedocs`
- the README renderer in `cmd/snapshotreadme`
- the reusable GitHub workflow in `.github/workflows/snapshot-sync.yml`
- optional Codex dry-run helpers under `.github/codex` and `.github/workflows/codex-dry-run.yml`

The intended consumers are thin snapshot repos such as:

- `claude-code-docs-archive`
- `claude-platform-docs-archive`

Those repos should contain only:

- raw mirrored Markdown files at repo root
- a generated `README.md`
- a tiny caller workflow

## Crawler behavior

The crawler is stdlib-only and supports common `llms.txt` formats:

- Markdown-link indexes
- plain URL-per-line indexes

It can write in two layouts:

- `nested`: compatibility mode for the old `source/` + `pages/<host>/...` structure
- `root`: mirror URLs directly into the output root and write the source index as `llms.txt`

It also supports release-backed conditional fetch state:

- reads a previous `manifest.json` asset with `-previous-manifest`
- writes a fresh release asset with `-manifest-out`
- sends both `If-None-Match` and `If-Modified-Since` when prior validators are available
- only mirrors `.md` URLs from `llms.txt`
- rejects `.md` URLs that actually return HTML instead of raw markdown
- reports skipped non-Markdown URLs in the manifest asset

## Local usage

Example against Claude Code docs in compatibility mode:

```bash
go run ./cmd/claudecodedocs \
  -source https://code.claude.com/docs/llms.txt \
  -out snapshot \
  -layout nested \
  -manifest-out snapshot/manifest.json
```

Example against Anthropic Developer Platform docs in root mode:

```bash
go run ./cmd/claudecodedocs \
  -source https://platform.claude.com/llms.txt \
  -out /tmp/platform-generated \
  -layout root \
  -manifest-out /tmp/platform-manifest.json
```

README rendering example:

```bash
go run ./cmd/snapshotreadme \
  -template templates/snapshot-readme.md.tmpl \
  -out /tmp/README.md \
  -title "Claude Platform Docs Archive" \
  -site-name "Claude Platform Docs" \
  -site-url "https://platform.claude.com" \
  -source-url "https://platform.claude.com/llms.txt" \
  -schedule-label "Hourly at :42 UTC" \
  -document-count 654 \
  -skipped-count 3 \
  -releases-json /tmp/releases.json
```

## Reusable workflow

Snapshot repos should call `.github/workflows/snapshot-sync.yml` with:

- `source_url`
- `site_name`
- `site_url`
- `repo_title`
- `schedule_label`
- `tool_ref`
- `tool_repo_token` when this tooling repo is private

The reusable workflow:

1. checks out the caller snapshot repo
2. checks out this tool repo at `tool_ref`
3. installs the crawler and README renderer
4. downloads the latest released `manifest.json` if one exists
5. crawls into a temp directory
6. syncs raw Markdown files into the snapshot repo root
7. renders the generated `README.md`
8. commits and creates a release only when raw tracked content actually changed

If this tooling repo is private, the caller repo must provide a secret with read access to `f-pisani/claudecodedocs` and pass it to the reusable workflow as `tool_repo_token`.

## Notes

- `manifest.json` is a release asset, not a git-tracked file in snapshot repos.
- Snapshot repos mirror only `.md` URLs listed in `llms.txt`.
- If any fetch fails, the workflow uploads diagnostics but does not publish a commit or release.
- Removed Markdown URLs are deleted from the tracked tree on successful syncs.

## Codex defaults

The optional Codex dry-run workflow defaults to:

- `CODEX_MODEL=gpt-5.4`
- `CODEX_EFFORT=high`

You only need to configure `OPENAI_API_KEY` for the default path. If you want to override the model or reasoning effort later, set repository variables:

- `CODEX_MODEL`
- `CODEX_EFFORT`
