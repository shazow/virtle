# Adapt a Docker image for virtle

A Docker image already contains most of the userspace needed for a development
VM. To boot it with virtle, add the pieces normally supplied by a container
runtime: a kernel and initramfs, an init process, and explicit VM
configuration.

This example adapts the official `alpine:3.24.1` image. It boots read-only,
starts QEMU Guest Agent (QGA), and opens a root shell on the serial console.
Use it as a template for an existing development image.

## Try the example

The script requires Podman, `virt-make-fs`, and QEMU. It assumes `virtle` is
already available. With Nix, the remaining tools are:

```sh
nix-shell -p podman file guestfs-tools qemu
```

Run:

```sh
./run.sh
```

At the guest prompt, confirm that virtle reached QGA and then shut down:

```sh
cat /run/virtle-ready
poweroff -f
```

The script builds the image, exports its merged filesystem, converts that
archive to a raw ext4 image, and launches it with
[manifest.toml](manifest.toml). Temporary containers and VM files are removed
on exit; the Podman image cache is retained.

## Adapt your image

Start with the accompanying [Dockerfile](Dockerfile), but replace `FROM` with
your image and digest. Then adapt these three guest-specific pieces:

1. Install a kernel, initramfs, and QGA appropriate for the distribution.
2. Arrange for the guest's init system to start QGA on
   `/dev/virtio-ports/org.qemu.guest_agent.0` and provide a serial console.
   This Alpine example uses the minimal [inittab](inittab) instead of OpenRC.
3. Update [manifest.toml](manifest.toml) with the exported disk, filesystem
   type, kernel parameters, memory, CPUs, networks, and any virtle-managed
   files or mounts.

The `[[write_files]]` marker in the manifest is deliberate: it makes virtle
wait for QGA readiness and includes the guest-agent stage in launch
instrumentation. It is written under a `/run` tmpfs so the root disk can remain
read-only.

The example targets `linux/amd64`. Keep the image, kernel, initramfs, and QEMU
guest architecture aligned. Also ensure that the initramfs contains the
drivers needed to discover the virtio disk and mount its filesystem.

## Translate container configuration

`podman export` captures the merged filesystem, not the image's runtime
metadata. Translate the parts your development environment relies on:

- `ENTRYPOINT` and `CMD` become an init service, serial shell, or virtle run
  command.
- Environment variables and the working directory must be configured inside
  the guest or by its init system.
- Declared volumes and bind mounts become virtle mounts.
- Published ports and container networks become guest networking or vsock
  services.
- Resource limits become the manifest's machine settings.

This boundary is useful: the image remains the source of the development
userspace, while the manifest describes how that userspace runs as a VM.

## Nix flake variant

[flake.nix](flake.nix) demonstrates the same conversion without Podman:

```sh
nix run
```

It uses `dockerTools` to fetch and flatten the digest-pinned Alpine image, adds
the pinned Alpine QGA package closure, and creates a read-only SquashFS root.
The kernel and initramfs are separate pinned netboot inputs, illustrating that
they do not need to live inside the container rootfs.

This variant is pure but more specialized: because `buildLayeredImage` cannot
run the image's `apk`, the QGA APK dependency closure is listed explicitly.
Nix must also have the `kvm` system feature and access to `/dev/kvm` for
`dockerTools.exportImage`.
