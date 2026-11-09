#!/usr/bin/env bash
# One-time privileged installation. Runtime clients do not need sudo.
set -euo pipefail
[[ $EUID == 0 ]] || { printf 'Run through sudo ask-exec\n' >&2; exit 1; }
client=${1:?Usage: install.sh CLIENT_USER STATIC_LAUNCHER}
launcher=${2:?Supply the prebuilt static cmd/service-launcher executable}
[[ $client =~ ^[a-z_][a-z0-9_-]*$ && $(id -u "$client") != 0 ]] || { printf 'Invalid non-root client\n' >&2; exit 1; }
group=$(id -gn "$client")
[[ $group =~ ^[a-z_][a-z0-9_-]*$ ]] || exit 1
repo=$(cd "$(dirname "$0")/.." && pwd)
[[ -x $launcher && -x $repo/assets/firecracker && -f $repo/assets/vmlinux ]] || { printf 'Missing launcher, VMM, or kernel\n' >&2; exit 1; }
for binary in "$launcher" "$repo/assets/firecracker"; do
  file "$binary" | python3 -c 'import sys; s=sys.stdin.read(); sys.exit(0 if "static" in s else "Executable must be static for the filesystem jail")'
done
[[ -e /dev/kvm && -e /dev/net/tun ]] || { printf 'KVM/TUN required\n' >&2; exit 1; }
[[ $(sysctl -n net.ipv4.ip_forward) == 1 ]] || { printf 'Host IPv4 forwarding must be enabled first\n' >&2; exit 1; }
if [[ ! -f /opt/firecracker-builder/managed-install ]]; then
  for target in /etc/systemd/system/firecracker-builder@.service /etc/systemd/system/firecracker-builder-network.service /etc/polkit-1/rules.d/49-firecracker-builder.rules /opt/firecracker-builder /var/lib/firecracker-builder /usr/local/libexec/firecracker-builder-launcher /usr/local/libexec/firecracker-builder-network; do
    [[ ! -e $target ]] || { printf 'Refusing existing unmanaged path: %s\n' "$target" >&2; exit 1; }
  done
fi
if systemctl is-active --quiet firecracker-builder@main.service; then printf 'Stop current VM before install\n' >&2; exit 1; fi
install -d -m 0755 -o root -g root /opt/firecracker-builder /usr/local/libexec /var/lib/firecracker-builder /var/lib/firecracker-builder/jail
install -d -m 0770 -o "$client" -g "$group" /var/lib/firecracker-builder/work
for dir in bin kernel work run/firecracker-builder dev/net usr/local/libexec proc sys; do
  install -d -m 0755 -o root -g root "/var/lib/firecracker-builder/jail/$dir"
done
install -m 0755 -o root -g root "$repo/assets/firecracker" /opt/firecracker-builder/firecracker
install -m 0644 -o root -g root "$repo/assets/vmlinux" /opt/firecracker-builder/vmlinux
install -m 0755 -o root -g root "$launcher" /usr/local/libexec/firecracker-builder-launcher

install -m 0755 -o root -g root "$repo/deployment/network.sh" /usr/local/libexec/firecracker-builder-network
install -m 0644 -o root -g root "$repo/deployment/firecracker-builder-network.service" /etc/systemd/system/firecracker-builder-network.service
python3 - "$repo" "$client" "$group" <<'PY'
import os,pathlib,sys
repo,user,group=sys.argv[1:]
for source,dest in [('deployment/firecracker-builder@.service','/etc/systemd/system/firecracker-builder@.service'),('deployment/49-firecracker-builder.rules','/etc/polkit-1/rules.d/49-firecracker-builder.rules')]:
    data=(pathlib.Path(repo)/source).read_text().replace('@CLIENT_USER@',user).replace('@CLIENT_GROUP@',group)
    p=pathlib.Path(dest);p.write_text(data);os.chmod(p,0o644);os.chown(p,0,0)
pathlib.Path('/opt/firecracker-builder/managed-install').write_text('firecracker-builder systemd installation\nclient='+user+'\n')
PY
systemd-analyze verify /etc/systemd/system/firecracker-builder@.service /etc/systemd/system/firecracker-builder-network.service
systemctl daemon-reload
printf 'Installed. Client: %s; unit: firecracker-builder@main.service; API: /run/firecracker-builder/main.sock\n' "$client"
printf 'Workspace: /var/lib/firecracker-builder/work (jail path /work). No unit was enabled at boot.\n'
