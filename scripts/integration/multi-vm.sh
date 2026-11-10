#!/usr/bin/env bash
# OPERATOR-RUN real multi-VM verification. NOT part of `make check`, and it has
# been exercised on the reference host (docs/verification-live.md). It talks to the
# running HTTP control API (vmapi) with an API bearer token and to guests through
# your ProxyJump bastion; it never uses sudo or systemctl. VM and image IDs are
# assigned by the API, so the script reads them from vmctl output.
#
# Flow: boot -> SSH -> external mutation -> stop -> commit NEW image ->
#       two independent starts -> persistence + independence -> base unchanged.
#
# Required env:
#   FC_INTEGRATION_CONFIRM=yes-run-on-real-host
#   FC_URL             API base URL (https, or loopback http)
#   FC_TOKEN_FILE      private (0600) file holding one bearer token (never an argv value)
#   FC_AUTH            optional: jwt (default) | user | service; FC_USER_ID for the last two
#   FC_CA_FILE         optional: root CA PEM for a private HTTPS API
#   FC_BASE_IMAGE_ID   image id of the base (e.g. ubuntu-base)
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
for v in FC_URL FC_TOKEN_FILE FC_BASE_IMAGE_ID FC_SSH_KEY; do
  [[ -n ${!v:-} ]] || { echo "missing $v" >&2; exit 2; }
done

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
run="it$(date +%s)"
golden="golden-$run"
vms=()       # API-assigned VM ids: src, a, b
created=()
vmctl() {
  local extra=()
  [[ -z ${FC_CA_FILE:-} ]] || extra+=(-ca-file "$FC_CA_FILE")
  [[ -z ${FC_USER_ID:-} ]] || extra+=(-user-id "$FC_USER_ID")
  "${FC_VMCTL:-$work/vmctl}" -url "$FC_URL" -token-file "$FC_TOKEN_FILE" -auth "${FC_AUTH:-jwt}" "${extra[@]}" "$@"
}
jget() { python3 -c 'import json,sys; d=json.load(sys.stdin)
for k in sys.argv[1].split("."): d=d[k]
print(d)' "$1"; }

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

# FC_VMCTL is only for the offline script test (scripts/integration/test_multi_vm.py).
[[ -n ${FC_VMCTL:-} ]] || (cd "$root" && CGO_ENABLED=0 go build -o "$work/vmctl" ./cmd/vmctl)
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
vms[0]=$(vmctl create -name "$run-src" -image "$FC_BASE_IMAGE_ID" 2>/dev/null | jget accepted.vm_id)
created+=("${vms[0]}")
wait_ssh "${vms[0]}"
echo "== 2. external mutation"
mutate "${vms[0]}" "echo $run > /root/it-marker && sync" FC_MUTATION_PLAYBOOK
echo "== 3. graceful stop + commit NEW image $golden"
vmctl stop "${vms[0]}" >/dev/null 2>&1
golden_id=$(vmctl commit "${vms[0]}" "$golden" 2>/dev/null | jget accepted.image_id)
if vmctl commit "${vms[0]}" "$golden" >/dev/null 2>&1; then echo "FAIL: overwrite accepted" >&2; exit 1; fi
echo "== 3b. delete the committed source so its slot/lease is free (max_vms may be 2)"
vmctl delete "${vms[0]}" >/dev/null 2>&1
created=()
echo "== 4. two independent VMs from $golden"
for n in a b; do
  v=$(vmctl create -name "$run-$n" -image "$golden_id" 2>/dev/null | jget accepted.vm_id)
  vms+=("$v")
  created+=("$v")
done
for v in "${vms[1]}" "${vms[2]}"; do
  wait_ssh "$v"
  [[ $(guest "$v" cat /root/it-marker) == "$run" ]] || { echo "FAIL: marker missing in $v" >&2; exit 1; }
done
ips=$(for v in "${vms[1]}" "${vms[2]}"; do
  vmctl ssh-info "$v" | jget host
done | sort -u | wc -l)
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
echo "PASS (new image $golden_id remains in the catalog; delete it with: vmctl delete-image $golden_id)"
