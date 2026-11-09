#!/usr/bin/env bash
# Boot a TEMPORARY COPY of an existing rootfs image. Requires network.sh up.
set -euo pipefail
cd "$(dirname "$0")"
[[ $(id -u) == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
IMAGE=${1:-images/ubuntu-postgres.ext4}
[[ -f "$IMAGE" ]] || { echo "Image not found: $IMAGE" >&2; exit 1; }
[[ -f assets/vmlinux && -x assets/firecracker ]] || { echo 'Run prepare-ubuntu.sh first.' >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp --reflink=auto --sparse=always "$IMAGE" "$work/running.ext4"
rootfs=$(realpath "$work/running.ext4")
kernel=$(realpath assets/vmlinux)
cat > "$work/config.json" <<JSON
{
  "boot-source": {"kernel_image_path": "$kernel", "boot_args": "console=ttyS0 reboot=k panic=1"},
  "drives": [{"drive_id": "rootfs", "path_on_host": "$rootfs", "is_root_device": true, "is_read_only": false}],
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024},
  "network-interfaces": [{"iface_id": "eth0", "guest_mac": "06:00:AC:10:00:02", "host_dev_name": "tap-fcpg"}]
}
JSON
echo 'Booting test VM. From another terminal: ssh -i assets/id_ed25519 root@172.16.0.2'
echo 'Run reboot inside the guest to terminate this VM.'
assets/firecracker --no-api --config-file "$work/config.json"
