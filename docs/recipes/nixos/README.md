# NixOS VM

The accompanying [flake.nix](flake.nix) builds a small NixOS disk image,
kernel, initrd, and virtle manifest. The guest includes public-key-only
OpenSSH, QEMU Guest Agent, and the virtio drivers required by virtle's
`microvm` machine.

It's simpler and more stand-alone version of what
[agentspace](https://github.com/shazow/agentspace) does.

Save `flake.nix` in a new directory, then build the image and launch into the
guest shell:

```sh
nix run
```

The app creates a disposable writable overlay for the immutable image, runs
the virtle package from its flake input, and removes the overlay after the VM
exits.

Password and keyboard-interactive authentication are disabled, so the initial
SSH connection fails immediately instead of prompting. Virtle then generates a
key under `.virtle/`, installs it through QEMU Guest Agent, and reconnects as
`root`.
