{
  description = "Boot the official Alpine Docker rootfs with virtle";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    virtle = {
      url = "github:shazow/virtle";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    kernel = {
      url = "file+https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/netboot-3.24.1/vmlinuz-virt";
      flake = false;
    };
    initramfs = {
      url = "file+https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/netboot-3.24.1/initramfs-virt";
      flake = false;
    };
  };

  outputs =
    {
      initramfs,
      kernel,
      nixpkgs,
      virtle,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      virtlePackage = virtle.packages.${system}.default;

      alpine = pkgs.dockerTools.pullImage {
        imageName = "alpine";
        imageDigest = "sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b";
        hash = "sha256-nK2IyUv9ZQ4v0dFKcTEZcQeWyrsUbN3OBzLDNhnAFn0=";
        arch = "amd64";
        finalImageName = "alpine";
        finalImageTag = "3.24.1";
      };

      layeredImage = pkgs.dockerTools.buildLayeredImage {
        name = "virtle-alpine-rootfs";
        tag = "3.24.1";
        fromImage = alpine;
        architecture = "amd64";
        extraCommands = ''
          mkdir -p etc
          cp ${./inittab} etc/inittab
        '';
      };

      # TODO: Use a lighter layer-flattening path if dockerTools gains one.
      rootfsTar = pkgs.dockerTools.exportImage {
        name = "virtle-alpine-rootfs.tar";
        fromImage = layeredImage;
        diskSize = 128;
      };

      rootImage =
        pkgs.runCommand "virtle-alpine-docker-root"
          {
            nativeBuildInputs = with pkgs; [
              fakeroot
              gnutar
              squashfsTools
            ];
          }
          ''
            mkdir root
            fakeroot -s fakeroot.state -- \
              tar --extract --file ${rootfsTar} --directory root --numeric-owner

            mkdir -p "$out"
            fakeroot -i fakeroot.state -- \
              mksquashfs \
                root \
                "$out/root.squashfs" \
                -noappend \
                -no-progress
          '';

      manifest = pkgs.writeText "manifest.toml" ''
        networks = []

        [machine]
        memory = 512
        vcpu = 8

        [kernel]
        path = "${kernel}"
        initrd_path = "${initramfs}"
        serial = "console"
        params = [
          "root=/dev/vda",
          "ro",
          "rootfstype=squashfs",
        ]

        [[mounts]]
        type = "image"
        source = "${rootImage}/root.squashfs"
        read_only = true
        image.format = "raw"
      '';

      runVirtle = pkgs.writeShellApplication {
        name = "virtle-alpine-docker";
        runtimeInputs = with pkgs; [
          coreutils
          qemu_kvm
        ];
        text = ''
          work_dir="$(mktemp -d "''${WORKSPACE:-''${TMPDIR:-/tmp}}/virtle-alpine-docker.XXXXXX")"
          trap 'rm -rf -- "$work_dir"' EXIT

          cp "${manifest}" "$work_dir/manifest.toml"
          cd "$work_dir"
          "${virtlePackage}/bin/virtle" manifest validate
          "${virtlePackage}/bin/virtle" launch -v
        '';
      };
    in
    {
      packages.${system} = {
        default = rootImage;
        inherit layeredImage rootfsTar;
      };

      apps.${system}.default = {
        type = "app";
        program = "${runVirtle}/bin/virtle-alpine-docker";
        meta.description = "Boot the official Alpine Docker rootfs with virtle";
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
