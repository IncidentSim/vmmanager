#!/usr/bin/env bash
# Linux x86_64 only. Downloads Firecracker and its upstream CI Ubuntu rootfs,
# injects a demo SSH key and creates an ext4 disk for the Go image builder.
set -euo pipefail
cd "$(dirname "$0")"

if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  echo 'This example requires an x86_64 Linux host.' >&2
  exit 1
fi
for command in curl grep sort tar unsquashfs ssh-keygen mkfs.ext4 truncate; do
  command -v "$command" >/dev/null || { echo "Missing dependency: $command" >&2; exit 1; }
done
mkdir -p assets

if [[ ! -x assets/firecracker ]]; then
  echo 'Downloading Firecracker release...'
  release_url='https://github.com/firecracker-microvm/firecracker/releases'
  version=$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${release_url}/latest")")
  tmp_release=$(mktemp -d)
  trap 'rm -rf "$tmp_release"' EXIT
  curl -fL "${release_url}/download/${version}/firecracker-${version}-x86_64.tgz" -o "$tmp_release/firecracker.tgz"
  tar -xzf "$tmp_release/firecracker.tgz" -C "$tmp_release"
  install -m 0755 "$tmp_release/release-${version}-x86_64/firecracker-${version}-x86_64" assets/firecracker
fi

s3='https://s3.amazonaws.com/spec.ccfc.min'
echo 'Finding recent Firecracker CI kernel and Ubuntu rootfs...'
ci_prefix=$(curl -fsSL "${s3}?list-type=2&prefix=firecracker-ci/&delimiter=/" |
  grep -oP '(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)' | sort | tail -1)
[[ -n "$ci_prefix" ]] || { echo 'CI prefix lookup failed' >&2; exit 1; }

kernel_key=$(curl -fsSL "${s3}?list-type=2&prefix=${ci_prefix}x86_64/vmlinux-" |
  grep -oP "(?<=<Key>)(${ci_prefix}x86_64/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" |
  sort -V | tail -1)
ubuntu_key=$(curl -fsSL "${s3}?list-type=2&prefix=${ci_prefix}x86_64/ubuntu-" |
  grep -oP "(?<=<Key>)(${ci_prefix}x86_64/ubuntu-[0-9]+\.[0-9]+\.squashfs)(?=</Key>)" |
  sort -V | tail -1)
[[ -n "$kernel_key" && -n "$ubuntu_key" ]] || { echo 'CI artifact lookup failed' >&2; exit 1; }

if [[ ! -s assets/vmlinux ]]; then
  curl -fL "$s3/$kernel_key" -o assets/vmlinux
fi
if [[ -s assets/ubuntu-base.ext4 ]]; then
  echo 'assets/ubuntu-base.ext4 already exists; not overwriting.'
  exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"; rm -rf "${tmp_release:-/nonexistent}"' EXIT
curl -fL "$s3/$ubuntu_key" -o "$work/ubuntu.squashfs"
unsquashfs -d "$work/rootfs" "$work/ubuntu.squashfs"
if [[ ! -f assets/id_ed25519 ]]; then
  ssh-keygen -q -t ed25519 -N '' -f assets/id_ed25519
fi
mkdir -p "$work/rootfs/root/.ssh"
cp assets/id_ed25519.pub "$work/rootfs/root/.ssh/authorized_keys"
chmod 700 "$work/rootfs/root/.ssh"
chmod 600 "$work/rootfs/root/.ssh/authorized_keys"
sudo chown -R root:root "$work/rootfs"

# PostgreSQL needs more space than the small demo image in upstream examples.
truncate -s 3G assets/ubuntu-base.ext4
sudo mkfs.ext4 -F -d "$work/rootfs" assets/ubuntu-base.ext4 >/dev/null
printf 'Prepared assets/firecracker, assets/vmlinux, assets/ubuntu-base.ext4 and SSH key.\n'
