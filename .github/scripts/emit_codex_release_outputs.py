#!/usr/bin/env python3

from __future__ import annotations

import json
import sys
from pathlib import Path


def write_output(name: str, value: str, github_output: Path) -> None:
    with github_output.open("a", encoding="utf-8") as handle:
        if "\n" in value:
            marker = f"__CODEX_{name.upper()}__"
            handle.write(f"{name}<<{marker}\n{value}\n{marker}\n")
        else:
            handle.write(f"{name}={value}\n")


def main() -> int:
    if len(sys.argv) != 4:
        print(
            "usage: emit_codex_release_outputs.py <summary-json> <output-dir> <github-output>",
            file=sys.stderr,
        )
        return 1

    summary_path = Path(sys.argv[1])
    output_dir = Path(sys.argv[2])
    github_output = Path(sys.argv[3])

    obj = json.loads(summary_path.read_text(encoding="utf-8"))
    output_dir.mkdir(parents=True, exist_ok=True)

    files = {
        "codex-commit-title.txt": str(obj["commit_title"]),
        "codex-commit-body.txt": str(obj["commit_body"]),
        "codex-release-title.txt": str(obj["release_title"]),
        "codex-release-notes.md": str(obj["release_notes_markdown"]),
        "codex-key-changes.txt": "\n".join(str(change) for change in obj["key_changes"]) + "\n",
    }
    for filename, content in files.items():
        (output_dir / filename).write_text(content, encoding="utf-8")

    write_output("commit_title", str(obj["commit_title"]), github_output)
    write_output("commit_body", str(obj["commit_body"]), github_output)
    write_output("release_title", str(obj["release_title"]), github_output)
    write_output("release_notes_file", str(output_dir / "codex-release-notes.md"), github_output)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
