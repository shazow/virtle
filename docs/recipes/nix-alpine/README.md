# Getting started with Alpine

The accompanying [flake.nix](flake.nix) pins Alpine's tiny BIOS cloud image,
replaces its OpenRC startup with a root shell on the serial console, and
extracts its kernel and initramfs for direct boot.

Save `flake.nix` in a new directory, then launch the guest:

```sh
nix run
```

The VM has eight vCPUs, 512 MiB of memory, no network devices, and an
immutable root filesystem. It intentionally has no SSH or QEMU Guest Agent
setup. At the `~ #` prompt, use `poweroff -f` to exit.
