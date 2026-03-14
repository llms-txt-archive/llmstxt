#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
prompt_file="$repo_root/.github/codex/prompts/docs-diff-summary.md"
validator_script="$repo_root/.github/scripts/validate_codex_release.py"
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
  "$artifact_dir/snapshot-diffstat.txt"; do
  if [ ! -f "$required_file" ]; then
    echo "missing required file: $required_file" >&2
    exit 1
  fi
done

isolated_workdir="$(mktemp -d "${TMPDIR:-/tmp}/claudecodedocs-codex-XXXXXX")"
mkdir -p "$isolated_workdir"
cp "$artifact_dir/snapshot-status.txt" "$isolated_workdir/"
cp "$artifact_dir/snapshot-diffstat.txt" "$isolated_workdir/"
if [ -f "$artifact_dir/snapshot-sanitized.patch" ]; then
  cp "$artifact_dir/snapshot-sanitized.patch" "$isolated_workdir/"
else
  perl -0777 -pe 's/<!--.*?-->//gs' "$artifact_dir/snapshot.patch" > "$isolated_workdir/snapshot-sanitized.patch"
fi

if [ -f "$artifact_dir/snapshot-context.json" ]; then
  cp "$artifact_dir/snapshot-context.json" "$isolated_workdir/"
else
  if [ ! -f "$artifact_dir/snapshot/manifest.json" ]; then
    echo "missing required file: $artifact_dir/snapshot-context.json (or fallback $artifact_dir/snapshot/manifest.json)" >&2
    exit 1
  fi

  document_count="$(jq -r '.document_count // 0' "$artifact_dir/snapshot/manifest.json")"
  skipped_count="$(jq -r '.skipped_count // 0' "$artifact_dir/snapshot/manifest.json")"
  failure_count="$(jq -r '.failures | length' "$artifact_dir/snapshot/manifest.json")"
  published_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  jq -n \
    --arg repository "local/codex-dry-run" \
    --arg site_name "Documentation Archive" \
    --arg site_url "" \
    --arg source_url "" \
    --arg release_tag "local-dry-run" \
    --arg published_at "$published_at" \
    --argjson document_count "$document_count" \
    --argjson skipped_count "$skipped_count" \
    --argjson failure_count "$failure_count" \
    '{
      repository: $repository,
      site_name: $site_name,
      site_url: $site_url,
      source_url: $source_url,
      release_tag: $release_tag,
      published_at: $published_at,
      document_count: $document_count,
      skipped_count: $skipped_count,
      failure_count: $failure_count
    }' > "$isolated_workdir/snapshot-context.json"
fi

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
  python3 "$validator_script" "$output_file"
}

run_codex() {
  local prompt_path="$1"
  codex "${codex_args[@]}" exec \
    --skip-git-repo-check \
    --ephemeral \
    -C "$isolated_workdir" \
    -s read-only \
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
    cat <<'EOF'

The previous JSON output did not satisfy the required schema or validation rules.
Re-read the four provided input files and return corrected JSON only.
Do not mention any rejection, validation, or prior output.
Do not mention the rejection.
Do not add commentary outside the schema fields.
EOF
  } > "$retry_prompt"

  current_prompt="$retry_prompt"
  attempt=$((attempt + 1))
done

echo "Wrote $output_file"
