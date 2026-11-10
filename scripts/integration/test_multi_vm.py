"""Offline sequencing/admission test for scripts/integration/multi-vm.sh.

A fake vmctl enforces max_vms=2 exactly like the real backend (a stopped VM keeps
its lease until deleted) and a fake ssh keeps per-VM "disk" state. This proves the
script's ORDER and admission behaviour only; it boots nothing.
"""
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent / "multi-vm.sh"

FAKE_VMCTL = r'''#!/usr/bin/env bash
set -euo pipefail
S="$FAKE_STATE"; MAX="${FAKE_MAX_VMS:-2}"
# drop the global connection flags
while [[ ${1:-} == -* && ${1:-} != -format && ${1:-} != -id ]]; do
  case $1 in -addr|-server-name|-ca|-cert|-key) shift 2 ;; *) break ;; esac
done
cmd=$1; shift
echo "vmctl $cmd $*" >> "$S/calls"
live() { ls "$S/vm" 2>/dev/null | wc -l; }
case $cmd in
  create)
    id=''; image=''
    while [[ $# -gt 0 ]]; do case $1 in -id) id=$2; shift 2;; -image) image=$2; shift 2;; -profile) shift 2;; *) shift;; esac; done
    mkdir -p "$S/vm"
    if [[ $(live) -ge $MAX ]]; then echo "resource exhausted: all $MAX slots leased" >&2; exit 1; fi
    echo running > "$S/vm/$id"
    n=$(( $(cat "$S/ipn" 2>/dev/null || echo 0) + 1 )); echo $n > "$S/ipn"
    echo "10.213.$n.2" > "$S/ip.$id"; echo "$id" > "$S/host.10.213.$n.2"
    if [[ -f "$S/image.$image" ]]; then cp "$S/image.$image" "$S/disk.$id"; else : > "$S/disk.$id"; fi
    echo "{\"ID\":\"$id\"}" ;;
  stop) echo stopped > "$S/vm/$1"; echo '{}' ;;
  commit)
    [[ $(cat "$S/vm/$1") == stopped ]] || { echo "not stopped" >&2; exit 1; }
    [[ ! -e "$S/image.$2" && $2 != base* && $2 != ubuntu-base ]] || { echo "already exists" >&2; exit 1; }
    cp "$S/disk.$1" "$S/image.$2"; echo "{\"ID\":\"$2\"}" ;;
  delete) rm -f "$S/vm/$1"; echo '{"Deleted": true}' ;;
  ssh-info)
    fmt=json; id=''
    while [[ $# -gt 0 ]]; do case $1 in -format) fmt=$2; shift 2;; -known-hosts|-key) shift 2;; *) id=$1; shift;; esac; done
    ip=$(cat "$S/ip.$id")
    if [[ $fmt == ssh ]]; then echo "-J jump@bastion.example.test -p 22 root@$ip";
    elif [[ $fmt == ansible ]]; then echo "$id ansible_host=$ip"
    else echo "  \"Host\": \"$ip\","; fi ;;
  *) echo "unexpected $cmd" >&2; exit 2 ;;
esac
'''

FAKE_SSH = r'''#!/usr/bin/env bash
set -euo pipefail
S="$FAKE_STATE"
args=("$@"); target=''; cmd=''
for i in "${!args[@]}"; do
  if [[ -z $target && ${args[i]} =~ ^[a-z]+@[0-9.]+$ ]]; then target=${args[i]}; cmd="${args[*]:$((i+1))}"; break; fi
done
ip="${target#*@}"
id=$(cat "$S/host.$ip")
echo "ssh $id: $cmd" >> "$S/calls"
d="$S/disk.$id"
case $cmd in
  true) exit 0 ;;
  "cat /root/it-marker") cat "$d" ;;
  "echo "*"> /root/it-marker"*) m=${cmd#echo }; m=${m%% *}; echo "$m" > "$d" ;;
  "rm -f /root/it-marker"*) : > "$d"; touch "$d.only" ;;
  "test -f /root/it-marker") [[ -s $d ]] ;;
  "test ! -e /root/only-a") [[ ! -e "$d.only" ]] ;;
  *) echo "unexpected ssh command: $cmd" >&2; exit 3 ;;
esac
'''


class IntegrationScriptTests(unittest.TestCase):
    def run_script(self, max_vms=2, confirm=True):
        d = tempfile.mkdtemp()
        state = Path(d, "state")
        state.mkdir()
        (state / "image.ubuntu-base").write_text("")
        bin_dir = Path(d, "bin")
        bin_dir.mkdir()
        for name, body in (("vmctl", FAKE_VMCTL), ("ssh", FAKE_SSH)):
            p = bin_dir / name
            p.write_text(body)
            p.chmod(0o700)
        env = {
            "PATH": f"{bin_dir}:/usr/bin:/bin",
            "FAKE_STATE": str(state),
            "FAKE_MAX_VMS": str(max_vms),
            "FC_VMCTL": str(bin_dir / "vmctl"),
            "FC_ADDR": "x:1", "FC_SERVER_NAME": "x", "FC_CA": "ca", "FC_CERT": "c", "FC_KEY": "k",
            "FC_BASE_IMAGE_ID": "ubuntu-base", "FC_PROFILE": "small", "FC_SSH_KEY": "/nonexistent-key",
            "HOME": d,
        }
        if confirm:
            env["FC_INTEGRATION_CONFIRM"] = "yes-run-on-real-host"
        proc = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True, timeout=120)
        calls = (state / "calls").read_text().splitlines() if (state / "calls").exists() else []
        return proc, calls, state

    def test_refuses_without_explicit_confirmation(self):
        proc, calls, _ = self.run_script(confirm=False)
        self.assertEqual(proc.returncode, 2)
        self.assertEqual(calls, [])

    def test_runs_under_a_two_vm_cap_by_deleting_the_committed_source_first(self):
        proc, calls, state = self.run_script(max_vms=2)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        verbs = [c.split()[1] for c in calls if c.startswith("vmctl")]
        delete_src = next(i for i, c in enumerate(calls) if c.startswith("vmctl delete") and "-src" in c)
        creates = [i for i, c in enumerate(calls) if c.startswith("vmctl create") and "golden-" in c]
        commit = next(i for i, c in enumerate(calls) if c.startswith("vmctl commit") and "-src" in c)
        self.assertEqual(len(creates), 2)
        self.assertLess(commit, delete_src)
        self.assertLess(delete_src, min(creates), "descendants must start only after the source lease is freed")
        self.assertEqual(verbs.count("create"), 3)
        self.assertEqual(list((state / "vm").glob("*")), [], "script must clean up every VM it created")

    def test_would_fail_if_the_source_were_still_leased(self):
        # With a cap of 1 the two descendants cannot both run: the script must fail loudly.
        proc, _, state = self.run_script(max_vms=1)
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(list((state / "vm").glob("*")), [], "failed run must still delete what it created")

    def test_overwrite_of_committed_image_must_be_refused(self):
        proc, calls, _ = self.run_script()
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        commits = [c for c in calls if c.startswith("vmctl commit")]
        self.assertEqual(len(commits), 2)  # second attempt to the same id is the overwrite probe


if __name__ == "__main__":
    unittest.main()
