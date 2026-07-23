# Getting started with NixOS

The accompanying [flake.nix](flake.nix) builds a small NixOS disk image,
kernel, initrd, and virtle manifest. The guest includes OpenSSH, QEMU Guest
Agent, and the virtio drivers required by virtle's `microvm` machine.

Save `flake.nix` in a new directory, then build the image and launch into the
guest shell:

```sh
nix run
```

The app creates a disposable writable overlay for the immutable image, runs
the virtle package from its flake input, and removes the overlay after the VM
exits.

During each disposable launch, SSH authentication fails once while virtle
generates a key under `.virtle/`, installs it through QEMU Guest Agent, and
reconnects as `root`.
