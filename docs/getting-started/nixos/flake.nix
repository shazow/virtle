{
  description = "Minimal NixOS guest for virtle";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    virtle = {
      url = "github:shazow/virtle";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      nixpkgs,
      virtle,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      virtlePackage = virtle.packages.${system}.default;

      nixos = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          (
            {
              lib,
              pkgs,
              ...
            }:
            {
              boot.loader.grub.enable = false;
              boot.initrd.availableKernelModules = [
                "virtio_blk"
                "virtio_mmio"
              ];
              boot.kernelModules = [
                "virtio_console"
                "vmw_vsock_virtio_transport"
              ];

              fileSystems."/" = {
                device = "/dev/vda";
                fsType = "ext4";
              };

              networking.hostName = "virtle";

              nix.enable = false;
              nixpkgs.flake = {
                setFlakeRegistry = false;
                setNixPath = false;
              };

              services.openssh.enable = true;

              services.qemuGuest.enable = true;
              systemd.services.qemu-guest-agent.path = with pkgs; [
                bash
                coreutils
                gnugrep
              ];

              documentation.enable = false;
              environment.defaultPackages = lib.mkForce [ ];
              environment.systemPackages = with pkgs; [
                bashInteractive
                coreutils
                gnugrep
              ];

              system.stateVersion = "25.11";
            }
          )
        ];
      };

      diskImage = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
        inherit pkgs;
        lib = nixpkgs.lib;
        config = nixos.config;
        additionalSpace = "128M";
        baseName = "root";
        copyChannel = false;
        format = "qcow2-compressed";
        installBootLoader = false;
        partitionTableType = "none";
      };

      kernel = "${nixos.config.system.build.kernel}/${nixos.config.system.boot.loader.kernelFile}";
      initrd = "${nixos.config.system.build.initialRamdisk}/${nixos.config.system.boot.loader.initrdFile}";

      manifest = pkgs.writeText "manifest.toml" ''
        [machine]
        memory = 1024

        [kernel]
        path = "${kernel}"
        initrd_path = "${initrd}"
        params = [
          "root=/dev/vda",
          "rw",
          "init=${nixos.config.system.build.toplevel}/init",
          "systemd.ssh_listen=vsock::22",
        ]

        [ssh]
        user = "root"
        autoprovision = true

        [[mounts]]
        type = "image"
        source = "root.qcow2"
        image.format = "qcow2"
      '';

      runVirtle = pkgs.writeShellApplication {
        name = "virtle-nixos";
        runtimeInputs = with pkgs; [
          coreutils
          openssh
          qemu_kvm
        ];
        text = ''
          work_dir="$(mktemp -d "''${TMPDIR:-/tmp}/virtle-nixos.XXXXXX")"
          trap 'rm -rf -- "$work_dir"' EXIT

          cp "${manifest}" "$work_dir/manifest.toml"
          qemu-img create -q -f qcow2 -F qcow2 \
            -b "${diskImage}/root.qcow2" \
            "$work_dir/root.qcow2"

          cd "$work_dir"
          "${virtlePackage}/bin/virtle" launch -v --ssh "$@"
        '';
      };
    in
    {
      apps.${system}.default = {
        type = "app";
        program = "${runVirtle}/bin/virtle-nixos";
        meta.description = "Launch the generated NixOS guest with virtle";
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
