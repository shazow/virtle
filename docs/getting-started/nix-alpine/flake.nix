{
  description = "Minimal Alpine console guest for virtle";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    virtle = {
      url = "github:shazow/virtle";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    alpine = {
      url = "file+https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/generic_alpine-3.24.1-x86_64-bios-tiny-r0.qcow2";
      flake = false;
    };
  };

  outputs =
    {
      alpine,
      nixpkgs,
      virtle,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      virtlePackage = virtle.packages.${system}.default;

      inittab = pkgs.writeText "inittab" ''
        # Skip OpenRC and launch a root shell on virtle's serial console.
        ttyS0::respawn:/sbin/getty -n -l /bin/sh -L 115200 ttyS0 dumb

        ::ctrlaltdel:/sbin/reboot
        ::shutdown:/bin/umount -a -r
      '';

      image =
        pkgs.runCommand "virtle-alpine-image"
          {
            nativeBuildInputs = with pkgs; [
              e2fsprogs
              qemu_kvm
            ];
          }
          ''
            mkdir -p "$out"
            qemu-img convert -O raw "${alpine}" "$out/root.raw"

            debugfs -w -R "rm /etc/inittab" "$out/root.raw"
            debugfs -w -R "write ${inittab} /etc/inittab" "$out/root.raw"
            debugfs -R "dump /boot/vmlinuz-virt $out/vmlinuz" "$out/root.raw"
            debugfs -R "dump /boot/initramfs-virt $out/initramfs" "$out/root.raw"
            e2fsck -fy "$out/root.raw"
          '';

      manifest = pkgs.writeText "manifest.toml" ''
        networks = []

        [machine]
        memory = 512
        vcpu = 8

        [kernel]
        path = "${image}/vmlinuz"
        initrd_path = "${image}/initramfs"
        serial = "console"
        params = [
          "root=/dev/vda",
          "ro",
          "rootfstype=ext4",
        ]

        [[mounts]]
        type = "image"
        source = "${image}/root.raw"
        read_only = true
        image.format = "raw"
      '';

      runVirtle = pkgs.writeShellApplication {
        name = "virtle-alpine";
        runtimeInputs = with pkgs; [
          coreutils
          qemu_kvm
        ];
        text = ''
          work_dir="$(mktemp -d "''${TMPDIR:-/tmp}/virtle-alpine.XXXXXX")"
          trap 'rm -rf -- "$work_dir"' EXIT

          cp "${manifest}" "$work_dir/manifest.toml"
          cd "$work_dir"
          "${virtlePackage}/bin/virtle" launch -v
        '';
      };
    in
    {
      apps.${system}.default = {
        type = "app";
        program = "${runVirtle}/bin/virtle-alpine";
        meta.description = "Launch a minimal Alpine guest on virtle's console";
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
