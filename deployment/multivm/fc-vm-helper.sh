#!/usr/bin/env bash
# PROPOSED narrow root helper (source only). The ONLY operation is:
#     fc-vm-helper start-slot N
# N is a bounded decimal. There is no stop/kill/restart verb, no path, no unit
# name and no command argument: a guest ends by its own graceful shutdown, and
# the library waits for the launcher's exit receipt. It never touches
# firecracker-builder@main.service or any interface.
#
# How the unprivileged vmd user may run this as root is an OPERATOR decision
# (for example one exact-command polkit/sudoers rule). It is not installed here.
set -euo pipefail
readonly SYSTEMCTL=/usr/bin/systemctl
readonly MAX_SLOTS=4   # keep equal to fc-vm-network.sh and vmd max_vms

usage() { printf 'Usage: %s start-slot N\n' "$0" >&2; exit 2; }
[[ $# -eq 2 && $1 == start-slot ]] || usage
slot=$2
[[ $slot =~ ^(0|[1-9][0-9]?)$ ]] || usage
(( 10#$slot < MAX_SLOTS )) || usage

unit="firecracker-vm@${slot}.service"
state=$("$SYSTEMCTL" show --property=ActiveState --value "$unit")
# Never restart or re-start a slot that might hold a running guest.
case $state in
  inactive|failed) ;;
  *) printf 'slot %s is %s; refusing\n' "$slot" "$state" >&2; exit 3 ;;
esac
exec "$SYSTEMCTL" start --no-ask-password "$unit"
