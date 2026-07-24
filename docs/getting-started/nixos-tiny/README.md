# Getting started with tiny NixOS

This example explores how close a genuine NixOS stage-2 system can get to the
Alpine console example.

The flake is a fair bit larger and more complex, but that's that's what it
takes to turn off a bunch of things. There's room for more reduction!

The accompanying [flake.nix](flake.nix) builds a direct
boot kernel, initrd, and compressed root image with no bootloader, networking,
SSH, QEMU Guest Agent, Nix, Bash, or Perl.

The guest runs systemd and the NixOS-generated system closure, but starts only
a BusyBox root shell on the serial console:

```sh
nix run
```

To keep the appliance small, the configuration assumes an ext4 root at
`/dev/vda`, loads only its required kernel modules, uses `systemdMinimal`, and
boots a dedicated target instead of the normal multi-user graph. It is not a
general-purpose NixOS base image; the stock NixOS kernel and systemd initrd also
leave a startup-time gap relative to Alpine's purpose-built image.

The VM uses the same 512 MiB memory and eight-vCPU configuration as the Alpine
example. Its writable disk is a disposable overlay removed when the VM exits.
Run `poweroff -f` from the shell to stop it.
