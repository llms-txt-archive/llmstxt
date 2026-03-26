from __future__ import annotations

import json
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
FIXTURES = Path(__file__).resolve().parent / "fixtures"
VALIDATOR = ROOT / ".github/scripts/validate_codex_release.py"
EMITTER = ROOT / ".github/scripts/emit_codex_release_outputs.py"


class CodexReleaseHelperTests(unittest.TestCase):
    def test_validator_accepts_valid_fixture(self) -> None:
        subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "valid-summary.json")],
            check=True,
            cwd=ROOT,
        )

    def test_validator_accepts_special_chars_fixture(self) -> None:
        subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "valid-special-chars-summary.json")],
            check=True,
            cwd=ROOT,
        )

    def test_validator_rejects_placeholder_fixture(self) -> None:
        proc = subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "invalid-placeholder-summary.json")],
            check=False,
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("placeholder", proc.stderr.lower())

    def test_validator_rejects_meta_control_fixture(self) -> None:
        proc = subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "invalid-meta-control-summary.json")],
            check=False,
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("instruction-like phrase", proc.stderr.lower())

    def test_validator_rejects_missing_sections_fixture(self) -> None:
        proc = subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "invalid-missing-sections-summary.json")],
            check=False,
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("missing section", proc.stderr.lower())

    def test_validator_rejects_malformed_title_fixture(self) -> None:
        proc = subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "invalid-malformed-title-summary.json")],
            check=False,
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("dangling", proc.stderr.lower())

    def test_validator_rejects_executable_content_fixture(self) -> None:
        proc = subprocess.run(
            ["python3", str(VALIDATOR), str(FIXTURES / "invalid-executable-summary.json")],
            check=False,
            capture_output=True,
            text=True,
            cwd=ROOT,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("executable command-style content", proc.stderr.lower())

    def test_emitter_writes_files_and_multiline_outputs(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            temp_path = Path(temp_dir)
            github_output = temp_path / "github_output.txt"

            subprocess.run(
                [
                    "python3",
                    str(EMITTER),
                    str(FIXTURES / "valid-summary.json"),
                    str(temp_path),
                    str(github_output),
                ],
                check=True,
                cwd=ROOT,
            )

            self.assertTrue((temp_path / "codex-commit-title.txt").exists())
            self.assertTrue((temp_path / "codex-commit-body.txt").exists())
            self.assertTrue((temp_path / "codex-release-title.txt").exists())
            self.assertTrue((temp_path / "codex-release-notes.md").exists())
            self.assertTrue((temp_path / "codex-key-changes.txt").exists())

            output_text = github_output.read_text(encoding="utf-8")
            self.assertIn("commit_title=sync: remove temporary validation note from overview intro", output_text)
            self.assertIn("commit_body<<__CODEX_COMMIT_BODY__", output_text)
            self.assertIn("release_notes_file=", output_text)

            body_text = (temp_path / "codex-commit-body.txt").read_text(encoding="utf-8")
            self.assertIn("temporary validation note", body_text)

            summary = json.loads((FIXTURES / "valid-summary.json").read_text(encoding="utf-8"))
            notes = (temp_path / "codex-release-notes.md").read_text(encoding="utf-8")
            self.assertEqual(notes, summary["release_notes_markdown"])


if __name__ == "__main__":
    unittest.main()
