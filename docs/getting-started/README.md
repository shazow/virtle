# Getting started

Here's how to boot a custom VM image with virtle.

For fully declarative examples, see the [NixOS guide](nixos/README.md), the
[tiny NixOS console guide](nixos-tiny/README.md), and the [console-only
Alpine guide](nix-alpine/README.md).

Feel free to create the VM image any way you like. Here's an example:

```sh
mkdir virtle-ubuntu && cd virtle-ubuntu

base=https://cloud-images.ubuntu.com/minimal/releases/resolute/release
image=ubuntu-26.04-minimal-cloudimg-amd64.img

curl -LO "$base/$image" -LO "$base/SHA256SUMS"
grep "[ *]$image$" SHA256SUMS | sha256sum --check
mv "$image" root.qcow2
```

Copy the kernel and early microcode archive out of the image:

```sh
kernel=$(virt-ls -a root.qcow2 /boot | grep '^vmlinuz-' | sort -V | tail -1)
virt-copy-out -a root.qcow2 "/boot/$kernel" /boot/microcode.cpio .
mv "$kernel" vmlinuz
mv microcode.cpio initrd.img
```

## Boot on the console

Save this minimal manifest as `manifest.toml`:

```toml
[machine]
memory = 1024

[kernel]
path = "vmlinuz"
initrd_path = "initrd.img"
serial = "console"
params = ["root=/dev/vda1", "rw"]

[[mounts]]
type = "image"
source = "root.qcow2"
image.format = "qcow2"
```

Validate the manifest and boot the VM:

```sh
virtle manifest validate
virtle launch -v
```

Ubuntu's serial login prompt confirms that the kernel and image booted
successfully. The cloud image has no password login configured; press
<kbd>Ctrl-C</kbd> to stop the VM.

## Add SSH key provisioning

Virtle can use QEMU Guest Agent (QGA) to install an SSH client key before
connecting. Add QGA and a host SSH key to the image:

```sh
virt-customize -a root.qcow2 \
  --install qemu-guest-agent \
  --run-command 'systemctl enable qemu-guest-agent.service' \
  --run-command 'ssh-keygen -A'
```

Then update `manifest.toml` to disable the serial console, enable SSH over
vsock, and request automatic key provisioning:

```toml
[machine]
memory = 1024

[kernel]
path = "vmlinuz"
initrd_path = "initrd.img"
params = ["root=/dev/vda1", "rw", "systemd.ssh_listen=vsock::22"]

[ssh]
user = "root"
autoprovision = true

[[mounts]]
type = "image"
source = "root.qcow2"
image.format = "qcow2"
```

Launch into the guest shell:

```sh
virtle manifest validate
virtle launch -v --ssh
```

On the first connection, SSH authentication fails once while virtle generates
a key under `.virtle/`, installs it through QGA, and reconnects.
