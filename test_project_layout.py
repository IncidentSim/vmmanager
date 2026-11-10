"""Repository entrypoints: safe checks without root, network or KVM."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent

class ProjectLayoutTests(unittest.TestCase):
    def test_no_root_shell_scripts(self):
        self.assertEqual(list(ROOT.glob("*.sh")), [])

    def test_moved_scripts_resolve_project_root(self):
        for name in ("scripts/run-image.sh", "scripts/demo-lifecycle.sh", "deployment/prepare-ubuntu.sh"):
            with self.subTest(script=name):
                text = (ROOT / name).read_text()
                # Execute just the script's directory setup, never its workload.
                setup = next(line for line in text.splitlines() if line.startswith("cd "))
                result = subprocess.run(["bash", "-c", setup + "; pwd", str(ROOT / name)],
                                        cwd=ROOT.parent, capture_output=True, text=True, check=True)
                self.assertEqual(Path(result.stdout.strip()), ROOT)

    def test_make_entrypoints_are_explicit_and_unprivileged(self):
        for target in ("build", "remote-client", "test", "vet", "check", "run", "verify", "demo"):
            with self.subTest(target=target):
                result = subprocess.run(["make", "--no-print-directory", "-n", target, "IMAGE=images/custom.ext4"],
                                        cwd=ROOT, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn("sudo", result.stdout)
                self.assertNotIn("systemctl", result.stdout)
                if target in ("run", "verify"):
                    self.assertIn("images/custom.ext4", result.stdout)

    def test_make_builds_real_binaries(self):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(["make", "--no-print-directory", "build", "remote-client", "BUILD_DIR=" + directory],
                                    cwd=ROOT, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            for binary in ("image-builder", "remote-client"):
                self.assertTrue(os.access(Path(directory) / binary, os.X_OK))

if __name__ == "__main__":
    unittest.main()
