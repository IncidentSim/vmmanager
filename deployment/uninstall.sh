#!/usr/bin/env bash
# Revoke only this deployment; keep all workspace disks and source artifacts.
set -euo pipefail
[[ $EUID == 0 && -f /opt/firecracker-builder/managed-install ]] || { printf 'Run through ask-exec on a managed installation\n' >&2; exit 1; }
systemctl stop firecracker-builder@main.service firecracker-builder-network.service
rm -f /etc/polkit-1/rules.d/49-firecracker-builder.rules
rm -f /etc/systemd/system/firecracker-builder@.service /etc/systemd/system/firecracker-builder-network.service
rm -f /usr/local/libexec/firecracker-builder-launcher /usr/local/libexec/firecracker-builder-network
rm -f /opt/firecracker-builder/firecracker /opt/firecracker-builder/vmlinux /opt/firecracker-builder/managed-install
systemctl daemon-reload
printf 'Uninstalled. Workspace and filesystem-jail directories retained at /var/lib/firecracker-builder.\n'
