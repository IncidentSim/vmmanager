import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
HELPER = (HERE / "fc-vm-helper.sh").read_text()


class HelperTests(unittest.TestCase):
    """Run the helper unprivileged with a recording fake systemctl.

    The script's absolute systemctl path is rewritten in a COPY; the shipped
    script has no environment override.
    """

    def run_helper(self, args, state="inactive"):
        with tempfile.TemporaryDirectory() as d:
            log = Path(d) / "calls"
            fake = Path(d) / "systemctl"
            fake.write_text(
                "#!/bin/sh\n"
                f'echo "$@" >> {log}\n'
                f'[ "$1" = show ] && echo {state}\n'
                "exit 0\n"
            )
            fake.chmod(0o700)
            script = Path(d) / "helper.sh"
            script.write_text(HELPER.replace("/usr/bin/systemctl", str(fake)))
            script.chmod(0o700)
            proc = subprocess.run([str(script), *args], capture_output=True, text=True)
            calls = log.read_text().splitlines() if log.exists() else []
            return proc.returncode, calls

    def test_start_slot_only_starts_the_exact_template_instance(self):
        rc, calls = self.run_helper(["start-slot", "2"])
        self.assertEqual(rc, 0)
        self.assertEqual(calls[-1], "start --no-ask-password firecracker-vm@2.service")
        self.assertEqual(len(calls), 2)

    def test_rejects_everything_else_without_calling_systemctl(self):
        bad = [
            [], ["start-slot"], ["stop-slot", "1"], ["start-slot", "4"], ["start-slot", "-1"],
            ["start-slot", "01"], ["start-slot", "1;reboot"], ["start-slot", "../1"],
            ["start-slot", "1", "extra"], ["restart", "main"], ["start-slot", "1 2"],
            ["start-slot", "main"], ["start-slot", ""],
        ]
        for args in bad:
            rc, calls = self.run_helper(args)
            self.assertNotEqual(rc, 0, args)
            self.assertEqual(calls, [], args)

    def test_refuses_a_slot_that_may_hold_a_running_guest(self):
        for state in ("active", "activating", "deactivating", "reloading"):
            rc, calls = self.run_helper(["start-slot", "1"], state=state)
            self.assertEqual(rc, 3, state)
            self.assertFalse(any(c.startswith("start ") for c in calls), state)

    def test_helper_has_no_kill_or_legacy_unit_references(self):
        code = "\n".join(l for l in HELPER.splitlines() if not l.strip().startswith("#"))
        for word in ("kill", "restart", " stop", "firecracker-builder", "sudo", "rm ", "eval"):
            self.assertNotIn(word, code, word)

    def test_scripts_are_executable_syntax_valid(self):
        for name in ("fc-vm-helper.sh", "fc-vm-network.sh"):
            subprocess.run(["bash", "-n", str(HERE / name)], check=True)


if __name__ == "__main__":
    unittest.main()
