# Boot an Alpine Docker image

A Docker image contains a userspace filesystem, but it does not contain
everything needed to boot a virtual machine. This example starts with the
official `alpine:3.24.1` image, adds Alpine's virtual-machine kernel and
initramfs, exports the container filesystem, and converts the export into an
ext4 disk that virtle can boot directly.

The resulting guest is intentionally small: it has no network devices, SSH,
QEMU Guest Agent, or OpenRC services. It mounts its root filesystem read-only
and opens a root shell on the serial console.

## Prerequisites

Install:

- Docker with access to a running daemon
- `virt-make-fs` from libguestfs
- QEMU with KVM support
- virtle

The example targets an x86_64 Linux host. The Docker build is fixed to
`linux/amd64` so that the exported root filesystem, kernel, and QEMU guest all
use the same architecture.

## Build and boot

Run the example from this directory:

```sh
./run.sh
```

The script performs these steps:

1. Build the accompanying [Dockerfile](Dockerfile), which installs
   `linux-virt` and configures a serial root shell.
2. Create a stopped container and copy its kernel and initramfs into a
   temporary working directory.
3. Export the merged container filesystem with `docker export`.
4. Use `virt-make-fs` to put the exported filesystem into a partitionless,
   raw ext4 image.
5. Validate [manifest.toml](manifest.toml) and launch the guest.

At the `~ #` prompt, inspect the Alpine filesystem or run:

```sh
poweroff -f
```

The script removes the temporary container and generated VM artifacts when it
exits. It keeps the built Docker image so subsequent runs can reuse Docker's
cache.

## What does not cross the boundary

`docker export` exports a container's merged filesystem. Docker image metadata
such as `CMD`, `ENTRYPOINT`, environment variables, declared volumes, and
exposed ports is not part of that archive. Once QEMU boots the filesystem, the
Linux kernel starts `/sbin/init`; for this minimal example, BusyBox init reads
the supplied [inittab](inittab) and starts the serial shell.

The virtle manifest replaces the remaining container-runtime configuration:
it selects the kernel and initramfs, attaches the exported filesystem as
`/dev/vda`, supplies kernel parameters, and connects the serial console.

## Nix flake prototype

The accompanying [flake.nix](flake.nix) explores the same conversion without a
Docker daemon:

```sh
nix run
```

This version uses `dockerTools.pullImage` to fetch the same digest-pinned
official image, `dockerTools.buildLayeredImage` to add the serial `inittab`,
and `dockerTools.exportImage` to flatten its layers into a root filesystem
archive. It then packs the root filesystem as a read-only SquashFS image. The
official netboot initramfs already includes SquashFS support, so this avoids
adding a second filesystem driver and makes the immutable nature of the
prototype explicit.

`buildLayeredImage` cannot run the base image's `apk` command while constructing
an added layer. Instead, the flake pins Alpine's matching official netboot
kernel and initramfs as separate inputs. This keeps the build pure and makes an
important direct-boot property explicit: the QEMU kernel and initramfs do not
need to live inside the root filesystem they boot.

The flake does not require Docker, but Nix must have the `kvm` system feature
and access to `/dev/kvm`, because `dockerTools.exportImage` uses a small build
VM to flatten the image.
