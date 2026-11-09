#!/usr/bin/env bash
# Boot a TEMPORARY COPY using the Go SDK runner. Requires network.sh up.
set -euo pipefail
cd "$(dirname "$0")"
[[ $(id -u) == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
IMAGE=${1:-images/ubuntu-postgres.ext4}
[[ -f "$IMAGE" ]] || { echo "Image not found: $IMAGE" >&2; exit 1; }
[[ -f assets/vmlinux && -x assets/firecracker ]] || { echo 'Run prepare-ubuntu.sh first.' >&2; exit 1; }
[[ -x ./image-builder ]] || { echo 'Run: go build -o image-builder .' >&2; exit 1; }
work=$(mktemp -d)
vm_pid=''
cleanup() {
  if [[ -n "$vm_pid" ]] && kill -0 "$vm_pid" 2>/dev/null; then
    kill -TERM "$vm_pid" 2>/dev/null || true
    wait "$vm_pid" || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cp --reflink=auto --sparse=always "$IMAGE" "$work/running.ext4"
echo 'Booting test VM. From another terminal: ssh -i assets/id_ed25519 root@172.16.0.2'
echo 'Run reboot inside the guest to terminate this VM.'
./image-builder -run-image "$work/running.ext4" &
vm_pid=$!
wait "$vm_pid"
vm_pid=''
