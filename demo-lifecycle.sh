#!/usr/bin/env bash
# Run as the authorized unprivileged user after service installation.
set -Eeuo pipefail
cd "$(dirname "$0")"
OUT="$PWD/images/ubuntu-postgres-demo.ext4"
REPORT="$PWD/recordings/demo-verification.json"
[[ ! -e "$OUT" ]] || { printf 'Refusing to overwrite %s\n' "$OUT" >&2; exit 1; }
mkdir -p recordings
cleanup() {
  status=$?
  trap - EXIT
  [[ ! -f "$OUT" ]] || rm -- "$OUT"
  printf '\nDEMO EXIT STATUS: %s\n' "$status"
  exit "$status"
}
trap cleanup EXIT
printf '\n=== Build in systemd-managed VM ===\n'
./image-builder -output "$OUT"
e2fsck -fn "$OUT"
printf '\n=== Two boots: SQL and persistence ===\n'
FC_VERIFY_IMAGE="$OUT" FC_VERIFY_REPORT="$REPORT" python3 -u ./verify-image.py
printf '\n=== Service state after clean shutdown ===\n'
systemctl --no-ask-password show --property=ActiveState,SubState,Result,ExecMainStatus,ExecMainCode firecracker-builder@main.service
printf '\nSUCCESS: build, SQL, reboot persistence verified.\n'
