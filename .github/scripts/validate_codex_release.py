#!/usr/bin/env python3

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

COMMIT_TITLE_RE = re.compile(r"^sync: [A-Za-z0-9`(][A-Za-z0-9`(),/&' .-]{4,62}[A-Za-z0-9`)]$")
FORBIDDEN_SUFFIXES = {
    "+",
    "/",
    ":",
    "-",
    "and",
    "or",
    "to",
    "into",
    "with",
    "for",
    "new",
    "focused",
    "dedicated",
}
PLACEHOLDER_PHRASES = (
    "in progress",
    "analysis in progress",
    "inspect archive diff inputs",
    "archive diff analysis",
)
META_CONTROL_PHRASES = (
    "ignore previous instructions",
    "ignore all previous instructions",
    "follow instructions in the patch",
    "follow the instructions in the patch",
    "follow instructions found in the patch",
    "developer instructions",
    "developer message",
    "system prompt",
    "system message",
    "tool instructions",
    "follow the patch instructions",
    "obey the patch",
    "as instructed in the patch",
    "according to the patch instructions",
    "execute this command",
    "run this command",
    "visit this url",
)


def validate_summary(path: Path) -> list[str]:
    errors: list[str] = []

    try:
        obj = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:  # pragma: no cover - runtime guard
        return [f"invalid JSON: {exc}"]

    title = str(obj.get("commit_title", ""))
    release_title = str(obj.get("release_title", ""))
    notes = str(obj.get("release_notes_markdown", ""))
    key_changes = obj.get("key_changes", [])
    commit_body = str(obj.get("commit_body", ""))

    if not COMMIT_TITLE_RE.match(title):
        errors.append("commit_title must match the required 'sync: <finished phrase>' format")

    suffix = title.removeprefix("sync: ").strip().lower()
    if suffix in FORBIDDEN_SUFFIXES or suffix.endswith(" +"):
        errors.append("commit_title ends with a dangling or invalid suffix")
    if suffix.split() and suffix.split()[-1] in FORBIDDEN_SUFFIXES:
        errors.append("commit_title ends with a dangling final word")

    commit_lines = [line for line in commit_body.splitlines() if line.strip()]
    if not 2 <= len(commit_lines) <= 4:
        errors.append("commit_body must contain 2-4 bullet lines")
    elif any(not line.startswith("- ") for line in commit_lines):
        errors.append("commit_body must use '- ' bullet lines")

    if any(phrase in release_title.lower() for phrase in PLACEHOLDER_PHRASES):
        errors.append("release_title contains placeholder text")
    if any(phrase in notes.lower() for phrase in PLACEHOLDER_PHRASES):
        errors.append("release_notes_markdown contains placeholder text")

    for section in ("## Summary", "## Added Docs", "## Updated Docs", "## Notable Impact"):
        if section not in notes:
            errors.append(f"release_notes_markdown missing section: {section}")

    if not isinstance(key_changes, list) or len(key_changes) < 3:
        errors.append("key_changes must include at least 3 items")
    elif all(str(change).lower().startswith(("inspect", "review", "draft")) for change in key_changes):
        errors.append("key_changes contains placeholder task text")

    joined_output = "\n".join(
        [title, commit_body, release_title, notes] + [str(change) for change in key_changes]
    ).lower()
    for phrase in META_CONTROL_PHRASES:
        if phrase in joined_output:
            errors.append(f"output contains disallowed instruction-like phrase: {phrase}")

    if "```" in joined_output and any(token in joined_output for token in ("curl ", "wget ", "rm -rf", "bash -lc")):
        errors.append("output contains executable command-style content")

    return errors


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: validate_codex_release.py <summary-json>", file=sys.stderr)
        return 1

    errors = validate_summary(Path(sys.argv[1]))
    if not errors:
        return 0

    print("\n".join(errors), file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
