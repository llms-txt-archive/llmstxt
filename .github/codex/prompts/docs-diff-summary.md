You are analyzing a documentation snapshot diff for an llms.txt-driven documentation archive repository.

The current working directory contains:

- `snapshot-status.txt`: changed paths
- `snapshot-diffstat.txt`: per-file diff summary
- `snapshot.patch`: full patch for the changed files
- `snapshot/manifest.json`: the generated manifest for the new snapshot
- `snapshot-context.json`: metadata about the archive repo and source site

Your job is to produce high-signal release-writing material from the diff.

Requirements:

1. Read `snapshot-context.json`, the status file, diffstat, and patch before writing.
2. Focus on user-visible documentation changes, not internal repo mechanics.
3. Treat this as a real docs snapshot change set. You are writing materials for snapshot changes only, not for source-code changes.
4. Prefer concrete statements over vague summary language.
5. Call out brand-new docs pages separately from edits to existing pages.
6. If a change looks like a rename, split, or restructure rather than a brand-new feature, say that.
7. If you are uncertain about intent, say so briefly instead of over-claiming.
8. Do not mention CI, artifacts, patches, hashes, or repository plumbing in the final wording unless absolutely necessary.
9. The commit title must reflect the actual documentation change, not generic "update docs" wording.

Output guidance:

- `commit_title`: one concise conventional-commit-style title using the exact format `sync: <what changed>`
- `commit_title` must be a complete finished phrase, not a fragment
- `commit_title` must not end with `+`, `/`, `:`, `-`, `and`, `or`, `to`, `into`, `with`, `for`, `new`, `focused`, or `dedicated`
- good `commit_title` examples:
  - `sync: split commands, env vars, and tools into dedicated refs`
  - `sync: move env var and tool tables into standalone refs`
- bad `commit_title` examples:
  - `sync: split commands, env vars, and tools into new reference +`
  - `sync: split command, tool, and env var references into focused`
- `commit_body`: 2-4 short bullet-style lines joined into a single string with newlines
- `release_title`: a short human-readable release heading
- `release_notes_markdown`: markdown with these sections if relevant:
  - `## Summary`
  - `## Added Docs`
  - `## Updated Docs`
  - `## Notable Impact`
- `key_changes`: a short array of the most important changes

Keep the writing tight, specific, and easy to scan.
