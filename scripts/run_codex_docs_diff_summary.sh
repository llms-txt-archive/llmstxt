#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
schema_file="$repo_root/.github/codex/schemas/docs-diff-summary.schema.json"
prompt_file="$repo_root/.github/codex/prompts/docs-diff-summary.md"
model="gpt-5.4"
artifact_dir=""
output_file=""
keep_workdir="false"
reasoning_effort="high"
max_attempts=2

usage() {
  echo "usage: $0 --artifact-dir <artifact-dir> [--model <model>] [--reasoning-effort <level>] [--output <output-json-file>] [--keep-workdir]" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --artifact-dir)
      artifact_dir="${2:-}"
      shift 2
      ;;
    --model)
      model="${2:-}"
      shift 2
      ;;
    --reasoning-effort)
      reasoning_effort="${2:-}"
      shift 2
      ;;
    --output)
      output_file="${2:-}"
      shift 2
      ;;
    --keep-workdir)
      keep_workdir="true"
      shift 1
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done

if [ -z "$artifact_dir" ]; then
  usage
  exit 1
fi

if [ -z "$output_file" ]; then
  sanitized_model="${model//[^A-Za-z0-9._-]/_}"
  output_file="$artifact_dir/codex-docs-summary.${sanitized_model}.json"
fi

for required_file in \
  "$artifact_dir/snapshot-status.txt" \
  "$artifact_dir/snapshot-diffstat.txt" \
  "$artifact_dir/snapshot.patch" \
  "$artifact_dir/snapshot/manifest.json"; do
  if [ ! -f "$required_file" ]; then
    echo "missing required file: $required_file" >&2
    exit 1
  fi
done

isolated_workdir="$(mktemp -d "${TMPDIR:-/tmp}/claudecodedocs-codex-XXXXXX")"
mkdir -p "$isolated_workdir/snapshot"
cp "$artifact_dir/snapshot-status.txt" "$isolated_workdir/"
cp "$artifact_dir/snapshot-diffstat.txt" "$isolated_workdir/"
cp "$artifact_dir/snapshot.patch" "$isolated_workdir/"
cp "$artifact_dir/snapshot/manifest.json" "$isolated_workdir/snapshot/manifest.json"

cleanup() {
  if [ "$keep_workdir" != "true" ] && [ -d "$isolated_workdir" ]; then
    rm -rf "$isolated_workdir"
  fi
}
trap cleanup EXIT

mkdir -p "$(dirname "$output_file")"

echo "Model: $model"
if [ -n "$reasoning_effort" ]; then
  echo "Reasoning effort: $reasoning_effort"
fi
echo "Isolated CI-like workdir: $isolated_workdir"
echo "Output: $output_file"

codex_args=(
  -m "$model"
  -a never
)

if [ -n "$reasoning_effort" ]; then
  codex_args+=(-c "model_reasoning_effort=\"$reasoning_effort\"")
fi

validate_output() {
  python3 - "$output_file" <<'PY'
import json
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
errors = []

try:
    obj = json.loads(path.read_text())
except Exception as exc:
    print(f"invalid JSON: {exc}")
    sys.exit(1)

title = obj.get("commit_title", "")
release_title = obj.get("release_title", "")
notes = obj.get("release_notes_markdown", "")
key_changes = obj.get("key_changes", [])

if not re.match(r"^docs\(snapshot\)!?: [A-Za-z0-9`(][A-Za-z0-9`(),/&' .-]{10,68}[A-Za-z0-9`)]$", title):
    errors.append("commit_title does not match the required semantic-title format")

suffix = title.removeprefix("docs(snapshot): ").strip().lower()
forbidden_suffixes = {
    "+", "/", ":", "-", "and", "or", "to", "into", "with", "for",
    "new", "focused", "dedicated",
}
if suffix in forbidden_suffixes or suffix.endswith(" +"):
    errors.append("commit_title ends with a dangling or invalid suffix")
if suffix.split() and suffix.split()[-1] in forbidden_suffixes:
    errors.append("commit_title ends with a dangling final word")
if re.search(r"\b(?:into|for|with|to|in)(?:a|an|the)$", suffix):
    errors.append("commit_title ends with a merged dangling preposition/article")

placeholder_phrases = (
    "in progress",
    "analysis in progress",
    "inspect snapshot diff inputs",
    "snapshot diff analysis",
)
if any(phrase in release_title.lower() for phrase in placeholder_phrases):
    errors.append("release_title is still a placeholder")
if any(phrase in notes.lower() for phrase in placeholder_phrases):
    errors.append("release_notes_markdown is still a placeholder")

required_sections = ("## Summary", "## Added Docs", "## Updated Docs")
for section in required_sections:
    if section not in notes:
        errors.append(f"release_notes_markdown is missing {section}")

if len(key_changes) < 3:
    errors.append("key_changes must include at least 3 items")
if key_changes and all(change.lower().startswith(("inspect", "review", "draft")) for change in key_changes):
    errors.append("key_changes are placeholder tasks, not actual documentation changes")

if errors:
    print("\n".join(errors))
    sys.exit(1)
PY
}

run_codex() {
  local prompt_path="$1"
  codex "${codex_args[@]}" exec \
    --skip-git-repo-check \
    --ephemeral \
    -C "$isolated_workdir" \
    -s read-only \
    --output-schema "$schema_file" \
    -o "$output_file" \
    - < "$prompt_path"
}

attempt=1
current_prompt="$prompt_file"
retry_prompt="$isolated_workdir/retry-prompt.md"

while [ "$attempt" -le "$max_attempts" ]; do
  run_codex "$current_prompt"

  if validation_errors="$(validate_output 2>&1)"; then
    break
  fi

  if [ "$attempt" -eq "$max_attempts" ]; then
    echo "$validation_errors" >&2
    exit 1
  fi

  {
    cat "$prompt_file"
    printf '\n\nThe previous JSON output was rejected for these reasons:\n'
    while IFS= read -r line; do
      printf -- '- %s\n' "$line"
    done <<< "$validation_errors"
    cat <<'EOF'

Return corrected JSON only.
Do not mention the rejection.
Do not add commentary outside the schema fields.
EOF
  } > "$retry_prompt"

  current_prompt="$retry_prompt"
  attempt=$((attempt + 1))
done

echo "Wrote $output_file"
