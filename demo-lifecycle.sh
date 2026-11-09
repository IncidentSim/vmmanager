#!/usr/bin/env bash
# Actual end-to-end demo. Run only via sudo ask-exec, optionally under script(1).
set -Eeuo pipefail
cd "$(dirname "$0")"
[[ $(id -u) == 0 ]] || { printf 'Run via sudo ask-exec bash ./demo-lifecycle.sh\n' >&2; exit 1; }
: "${TMPDIR:?Set TMPDIR to a scratch directory}"
OUT="$PWD/images/ubuntu-postgres-demo.ext4"
REPORT="$PWD/recordings/demo-verification.json"
[[ ! -e "$OUT" ]] || { printf 'Refusing to overwrite %s\n' "$OUT" >&2; exit 1; }
if ip link show tap-fcpg >/dev/null 2>&1; then
  printf 'tap-fcpg already exists; refusing to touch an existing VM network.\n' >&2
  exit 1
fi
mkdir -p recordings
network_owned=0
cleanup() {
  status=$?
  trap - EXIT
  if (( network_owned )); then bash ./network.sh down; fi
  # This path is dedicated to this run and was absent at startup.
  if [[ -f "$OUT" ]]; then rm -- "$OUT"; fi
  chown 1000:1000 recordings
  [[ ! -f "$REPORT" ]] || chown 1000:1000 "$REPORT"
  printf '\nDEMO EXIT STATUS: %s\n' "$status"
  exit "$status"
}
trap cleanup EXIT
PS4='+ host: '
set -x
printf '\n=== 1. Host KVM and prepared artifacts ===\n'
python3 -c 'import os,fcntl; f=os.open("/dev/kvm",os.O_RDWR); print("KVM API:",fcntl.ioctl(f,0xAE00,0)); v=fcntl.ioctl(f,0xAE01,0); print("KVM_CREATE_VM: OK"); os.close(v); os.close(f)'
assets/firecracker --version
stat -c '%n: %s bytes' assets/vmlinux assets/ubuntu-base.ext4
printf '\n=== 2. TAP and guest outbound NAT ===\n'
network_owned=1
bash ./network.sh up
ip -br addr show tap-fcpg
printf '\n=== 3. Build image: boot Ubuntu, install PostgreSQL, SQL check, clean shutdown ===\n'
./image-builder -output "$OUT"
e2fsck -fn "$OUT"
printf '\n=== 4. Release builder network before independent image boot test ===\n'
bash ./network.sh down
network_owned=0
printf '\n=== 5. Boot built image twice: INSERT, full VM restart, SELECT persisted row ===\n'
FC_VERIFY_IMAGE="$OUT" FC_VERIFY_REPORT="$REPORT" python3 -u ./verify-image.py
printf '\n=== 6. Verify cleanup and unchanged previously published image ===\n'
if ip link show tap-fcpg >/dev/null 2>&1; then
  printf 'ERROR: TAP remains\n' >&2
  exit 1
fi
printf 'tap-fcpg: absent\n'
for rule in \
  'nat POSTROUTING -o ens1 -s 172.16.0.2/32 -j MASQUERADE' \
  'filter FORWARD -i tap-fcpg -o ens1 -j ACCEPT' \
  'filter FORWARD -i ens1 -o tap-fcpg -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT'; do
  read -r -a args <<< "$rule"
  table=${args[0]}
  if iptables -t "$table" -C "${args[@]:1}" 2>/dev/null; then
    printf 'ERROR: demo firewall rule remains: %s\n' "$rule" >&2
    exit 1
  fi
done
printf 'Demo NAT/FORWARD rules: absent\n'
sha256sum images/ubuntu-postgres.ext4
printf '\nSUCCESS: build, PostgreSQL SQL, reboot persistence and network cleanup verified.\n'
set +x
