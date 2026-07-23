# Getting started

Here's how to boot a custom VM image with virtle.

First, we'll need a VM image with qemu-guest-agent (QGA) and an SSH server.
Virtle uses QGA to coordinate the guest remotely for things like
auto-provisioning SSH client keys.

Feel free to create the VM image any way you like. Here's an example:

```sh
mkdir virtle-ubuntu && cd virtle-ubuntu

base=https://cloud-images.ubuntu.com/minimal/releases/resolute/release
image=ubuntu-26.04-minimal-cloudimg-amd64.img

curl -LO "$base/$image" -LO "$base/SHA256SUMS"
grep "[ *]$image$" SHA256SUMS | sha256sum --check
mv "$image" root.qcow2

# Add QGA and a host SSH key (not auto-generated in this case)
virt-customize -a root.qcow2 \
  --install qemu-guest-agent \
  --run-command 'systemctl enable qemu-guest-agent.service' \
  --run-command 'ssh-keygen -A'
```

Copy the kernel and early microcode archive out of the image:

```sh
kernel=$(virt-ls -a root.qcow2 /boot | grep '^vmlinuz-' | sort -V | tail -1)
virt-copy-out -a root.qcow2 "/boot/$kernel" /boot/microcode.cpio .
mv "$kernel" vmlinuz
mv microcode.cpio initrd.img
```

Save this manifest as `manifest.toml`:

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

Validate the manifest and launch into the guest shell:

```sh
virtle manifest validate
virtle launch -v --ssh
```

On the first connection, SSH authentication fails once while virtle generates a
key under `.virtle/`, installs it through QEMU Guest Agent, and reconnects.
