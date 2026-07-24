#!/bin/sh
set -eu

for command in docker virt-make-fs virtle; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 1
  fi
done

if ! docker info >/dev/null 2>&1; then
  echo "cannot connect to the Docker daemon" >&2
  exit 1
fi

example_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
scratch_root=${WORKSPACE:-${TMPDIR:-/tmp}}
work_dir=$(mktemp -d "$scratch_root/virtle-docker-image.XXXXXX")
container=

cleanup() {
  if [ -n "$container" ]; then
    docker rm "$container" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT INT TERM

image=virtle-alpine-rootfs:3.24.1
docker build \
  --platform linux/amd64 \
  --tag "$image" \
  "$example_dir"

container=$(docker create --platform linux/amd64 "$image")
docker cp "$container:/boot/vmlinuz-virt" "$work_dir/vmlinuz"
docker cp "$container:/boot/initramfs-virt" "$work_dir/initramfs"
docker export --output "$work_dir/rootfs.tar" "$container"
docker rm "$container" >/dev/null
container=

virt-make-fs \
  --type=ext4 \
  --format=raw \
  --size=+32M \
  "$work_dir/rootfs.tar" \
  "$work_dir/root.raw"

cp "$example_dir/manifest.toml" "$work_dir/manifest.toml"
cd "$work_dir"
virtle manifest validate
virtle launch -v
