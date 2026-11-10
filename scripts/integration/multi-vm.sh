#!/usr/bin/env bash
# OPERATOR-RUN real multi-VM verification. NOT part of `make check`, and it has
# not been executed by the author of this change. It talks to a running vmd
# facade over mTLS and to guests through your ProxyJump bastion; it never uses
# sudo or systemctl.
#
# Flow: boot -> SSH -> external mutation -> stop -> commit NEW image ->
#       two independent starts -> persistence + independence -> base unchanged.
#
# Required env:
#   FC_INTEGRATION_CONFIRM=yes-run-on-real-host
#   FC_ADDR FC_SERVER_NAME FC_CA FC_CERT FC_KEY   facade address and client PKI
#   FC_BASE_IMAGE_ID   image id of the base (e.g. ubuntu-base)
#   FC_PROFILE         profile name (e.g. small)
#   FC_SSH_KEY         client-local private key that logs into the base image
# Optional:
#   FC_BASE_IMAGE_PATH readable path of the base file on THIS machine (hash check;
#                      run on the host or skip the hash check)
#   FC_MUTATION_PLAYBOOK / FC_BREAKAGE_PLAYBOOK  external ansible playbooks; when
#                      set and ansible-playbook exists they replace the built-in
#                      ssh mutations (Ansible is run by YOU, not by this project)
set -euo pipefail

[[ ${FC_INTEGRATION_CONFIRM:-} == yes-run-on-real-host ]] || {
  echo "Refusing: set FC_INTEGRATION_CONFIRM=yes-run-on-real-host after operator approval." >&2; exit 2; }
for v in FC_ADDR FC_SERVER_NAME FC_CA FC_CERT FC_KEY FC_BASE_IMAGE_ID FC_PROFILE FC_SSH_KEY; do
  [[ -n ${!v:-} ]] || { echo "missing $v" >&2; exit 2; }
done

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
run="it$(date +%s)"
golden="golden-$run"
vms=("$run-src" "$run-a" "$run-b")
created=()
vmctl() { "$work/vmctl" -addr "$FC_ADDR" -server-name "$FC_SERVER_NAME" -ca "$FC_CA" -cert "$FC_CERT" -key "$FC_KEY" "$@"; }

cleanup() {
  local rc=$?
  for v in "${created[@]:-}"; do
    [[ -n $v ]] || continue
    vmctl delete "$v" >/dev/null 2>&1 || echo "WARNING: $v not deleted (retained?) - inspect with vmctl get $v" >&2
  done
  rm -rf "$work"
  exit $rc
}
trap cleanup EXIT

(cd "$root" && CGO_ENABLED=0 go build -o "$work/vmctl" ./cmd/vmctl)
[[ -z ${FC_BASE_IMAGE_PATH:-} ]] || base_before=$(sha256sum "$FC_BASE_IMAGE_PATH" | cut -d' ' -f1)

guest() { # guest VM COMMAND... : ssh through the bastion with a per-VM known_hosts
  local vm=$1; shift
  local args
  eval "args=($(vmctl ssh-info -format ssh -known-hosts "$work/$vm.known_hosts" -key "$FC_SSH_KEY" "$vm"))"
  ssh -T -o BatchMode=yes -o ConnectTimeout=5 "${args[@]}" "$@"
}
wait_ssh() {
  for _ in $(seq 1 60); do guest "$1" true 2>/dev/null && return 0; sleep 2; done
  echo "SSH never became ready for $1" >&2; return 1
}
mutate() { # mutate VM 'shell command' [playbook-var]
  local vm=$1 cmd=$2 playbook=${3:-}
  if [[ -n $playbook && -n ${!playbook:-} ]] && command -v ansible-playbook >/dev/null; then
    local inv="$work/$vm.inventory"
    { echo "[target]"; vmctl ssh-info -format ansible -key "$FC_SSH_KEY" "$vm"; } > "$inv"
    ansible-playbook -i "$inv" "${!playbook}"
  else
    guest "$vm" "$cmd"
  fi
}

echo "== 1. boot source VM from base ${FC_BASE_IMAGE_ID}"
created+=("${vms[0]}")
vmctl create -id "${vms[0]}" -image "$FC_BASE_IMAGE_ID" -profile "$FC_PROFILE" >/dev/null
wait_ssh "${vms[0]}"
echo "== 2. external mutation"
mutate "${vms[0]}" "echo $run > /root/it-marker && sync" FC_MUTATION_PLAYBOOK
echo "== 3. graceful stop + commit NEW image $golden"
vmctl stop "${vms[0]}" >/dev/null
vmctl commit "${vms[0]}" "$golden" >/dev/null
if vmctl commit "${vms[0]}" "$golden" >/dev/null 2>&1; then echo "FAIL: overwrite accepted" >&2; exit 1; fi
echo "== 4. two independent VMs from $golden"
for v in "${vms[1]}" "${vms[2]}"; do
  created+=("$v")
  vmctl create -id "$v" -image "$golden" -profile "$FC_PROFILE" >/dev/null
done
for v in "${vms[1]}" "${vms[2]}"; do
  wait_ssh "$v"
  [[ $(guest "$v" cat /root/it-marker) == "$run" ]] || { echo "FAIL: marker missing in $v" >&2; exit 1; }
done
ips=$(for v in "${vms[1]}" "${vms[2]}"; do vmctl ssh-info "$v" | grep '"Host"'; done | sort -u | wc -l)
[[ $ips -eq 2 ]] || { echo "FAIL: VMs share an address" >&2; exit 1; }
echo "== 5. breakage on one VM must not affect the other"
mutate "${vms[1]}" "rm -f /root/it-marker && echo broken > /root/only-a && sync" FC_BREAKAGE_PLAYBOOK
guest "${vms[2]}" test -f /root/it-marker
guest "${vms[2]}" test ! -e /root/only-a
echo "== 6. base unchanged"
if [[ -n ${base_before:-} ]]; then
  [[ $(sha256sum "$FC_BASE_IMAGE_PATH" | cut -d' ' -f1) == "$base_before" ]] || { echo "FAIL: base image changed" >&2; exit 1; }
else
  echo "(base hash check skipped: FC_BASE_IMAGE_PATH not set)"
fi
echo "PASS (new image $golden remains in the catalog; delete it manually if unwanted)"
