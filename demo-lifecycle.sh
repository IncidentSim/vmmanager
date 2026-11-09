#!/usr/bin/env bash
# Run as the authorized unprivileged user after service installation.
set -Eeuo pipefail
cd "$(dirname "$0")"
SOCKET=${FC_SOCKET:-/run/firecracker-builder/main.sock}
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
./image-builder -socket "$SOCKET" -output "$OUT"
e2fsck -fn "$OUT"
printf '\n=== Two boots: SQL and persistence ===\n'
FC_VERIFY_IMAGE="$OUT" FC_VERIFY_REPORT="$REPORT" python3 -u ./verify-image.py -socket "$SOCKET"
printf '\n=== Clean exits confirmed by per-process receipts; service remains active ===\n'
python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); assert r["verified"] and r["clean_exit_receipts"]==2 and r["output_image_unchanged"]; print(json.dumps(r,indent=2))' "$REPORT"
printf '\nSUCCESS: build, SQL, reboot persistence verified.\n'
