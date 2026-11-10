#!/usr/bin/env bash
# Render the proposed multi-VM units into a throwaway root and run the real
# `systemd-analyze verify` on them. Nothing is installed, started or enabled and
# no privileges are used. Skips LOUDLY (exit 0 with a message) only when
# systemd-analyze is absent, so the absence is visible rather than a silent pass.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/deployment/multivm"

if ! command -v systemd-analyze >/dev/null 2>&1; then
  echo "SKIPPED: systemd-analyze not found; unit files were NOT verified by systemd" >&2
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
unitdir="$tmp/etc/systemd/system"
mkdir -p "$unitdir" "$tmp/usr/local/libexec" "$tmp/etc/firecracker-vms" "$tmp/dev"

# Stub executables so ExecStart= paths resolve inside the throwaway root.
for exe in fc-vm-launcher fc-vm-network fc-vm-helper; do
  printf '#!/bin/sh\nexit 0\n' > "$tmp/usr/local/libexec/$exe"
  chmod 755 "$tmp/usr/local/libexec/$exe"
done
cp "$src/pool.conf.example" "$tmp/etc/firecracker-vms/pool.conf"

group="$(id -gn)"
for f in firecracker-vm@.service firecracker-vm-network@.service fc-vm-helper@.service fc-vm-helper.socket; do
  sed "s/@CLIENT_GROUP@/$group/g" "$src/$f" > "$unitdir/$f"
done

# Stub base targets: units carry default dependencies on them, and the throwaway
# root has no systemd of its own. These stubs are never installed anywhere.
for t in sysinit basic shutdown sockets multi-user local-fs umount network; do
  printf '[Unit]\nDescription=stub %s target\n' "$t" > "$unitdir/$t.target"
done
printf '[Unit]\nDescription=stub slice\n' > "$unitdir/system-firecracker\\x2dbuilder.slice"

echo "systemd-analyze: $(systemd-analyze --version | head -1)"
systemd-analyze verify --root="$tmp" --man=no \
  "$unitdir/firecracker-vm@0.service" \
  "$unitdir/firecracker-vm-network@0.service" \
  "$unitdir/fc-vm-helper.socket" \
  "$unitdir/fc-vm-helper@1.service"
echo "systemd-analyze verify: OK (rendered units only; not installed)"
