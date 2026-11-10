#!/usr/bin/env bash
# Boot a disposable workspace copy through the unprivileged service client.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${1:-images/ubuntu-postgres.ext4}
SOCKET=${FC_SOCKET:-/run/firecracker-builder/main.sock}
[[ -f "$IMAGE" ]] || { echo "Image not found: $IMAGE" >&2; exit 1; }
[[ -x ./image-builder ]] || { echo 'Run: go build -o image-builder .' >&2; exit 1; }
python3 ./verify-image.py -socket "$SOCKET" --check-ready >/dev/null
work=$(mktemp -d /var/lib/firecracker-builder/work/run-XXXXXXXX)
chmod 0770 "$work"
vm_pid=''
safe_cleanup=1
cleanup() {
  if [[ -n "$vm_pid" ]]; then
    kill -TERM "$vm_pid" 2>/dev/null || true
    if wait "$vm_pid"; then safe_cleanup=1; else safe_cleanup=0; fi
  fi
  if (( safe_cleanup )); then rm -rf -- "$work";
  else printf 'Working disk retained for safety: %s\n' "$work" >&2; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp --reflink=auto --sparse=always "$IMAGE" "$work/running.ext4"
chmod 0660 "$work/running.ext4"
echo 'SSH: ssh -i assets/id_ed25519 root@172.16.0.2; reboot guest to exit cleanly.'
safe_cleanup=0
./image-builder -socket "$SOCKET" -run-image "$work/running.ext4" &
vm_pid=$!
if wait "$vm_pid"; then status=0; safe_cleanup=1; else status=$?; fi
vm_pid=''
exit "$status"
