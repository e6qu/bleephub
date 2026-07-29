from __future__ import annotations

import importlib.util
import json
import pathlib
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("check-codeql-sarif.py")
SPEC = importlib.util.spec_from_file_location("check_codeql_sarif", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class CodeQLSarifPolicyTest(unittest.TestCase):
    def write_sarif(self, directory: pathlib.Path, results: list[dict]) -> None:
        (directory / "results.sarif").write_text(
            json.dumps({"version": "2.1.0", "runs": [{"results": results}]}),
            encoding="utf-8",
        )

    def test_clean_sarif_passes(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            self.write_sarif(directory, [])
            self.assertEqual(MODULE.sarif_findings(directory), [])
            self.assertEqual(MODULE.main(["check-codeql-sarif.py", raw_directory]), 0)

    def test_finding_fails_and_summary_stays_on_one_line(self) -> None:
        finding = {
            "ruleId": "go/log-injection",
            "level": "error",
            "message": {"text": "unsafe\n::notice::forged"},
            "locations": [
                {
                    "physicalLocation": {
                        "artifactLocation": {"uri": "server.go"},
                        "region": {"startLine": 42},
                    }
                }
            ],
        }
        with tempfile.TemporaryDirectory() as raw_directory:
            directory = pathlib.Path(raw_directory)
            self.write_sarif(directory, [finding])
            self.assertEqual(MODULE.sarif_findings(directory), [finding])
            self.assertEqual(MODULE.main(["check-codeql-sarif.py", raw_directory]), 1)
        summary = MODULE.finding_summary(finding)
        self.assertNotIn("\n", summary)
        self.assertIn(r"\n::notice::forged", summary)

    def test_missing_sarif_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as raw_directory:
            with self.assertRaisesRegex(RuntimeError, "no SARIF files"):
                MODULE.sarif_findings(pathlib.Path(raw_directory))
            self.assertEqual(MODULE.main(["check-codeql-sarif.py", raw_directory]), 1)


if __name__ == "__main__":
    unittest.main()
