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

      fetchAlpinePackage =
        {
          hash,
          name,
          repo ? "main",
        }:
        pkgs.fetchurl {
          url = "https://dl-cdn.alpinelinux.org/alpine/v3.24/${repo}/x86_64/${name}";
          inherit hash;
        };

      qgaPackages = [
        (fetchAlpinePackage {
          name = "qemu-guest-agent-11.0.1-r0.apk";
          repo = "community";
          hash = "sha256-wTYxknrjVFapYs0egU/8KJYTXPDJFNX3LGJ1tyNkPe4=";
        })
        (fetchAlpinePackage {
          name = "glib-2.88.1-r1.apk";
          hash = "sha256-LzIgO5d4m8mpLlfi2biesuaVG2lTS6CCpoPG5JAxy10=";
        })
        (fetchAlpinePackage {
          name = "numactl-2.0.19-r0.apk";
          hash = "sha256-SnhnChbZrnN3kG4ghL50MBl9S0wkh31lcS3R1Fl7Wds=";
        })
        (fetchAlpinePackage {
          name = "liburing-2.14-r0.apk";
          hash = "sha256-d7Fwt03Q0to+70pgz3USG69t47MI+LPbiAjU5pdgJ3I=";
        })
        (fetchAlpinePackage {
          name = "libffi-3.5.2-r1.apk";
          hash = "sha256-baGhkxOkDFbKx1b7EeLE1SZ4MeHCL8gyPyTJQ8lNpuA=";
        })
        (fetchAlpinePackage {
          name = "libintl-1.0-r0.apk";
          hash = "sha256-ywORnLpWJLXodZ0E87MZE7AlhTVZiLkUvDHeR0b/XvY=";
        })
        (fetchAlpinePackage {
          name = "libmount-2.42.1-r0.apk";
          hash = "sha256-/G/QbBJEuAT8BxbUl4xKl4OtKkYbMeG0lJLU38v/sCs=";
        })
        (fetchAlpinePackage {
          name = "pcre2-10.47-r1.apk";
          hash = "sha256-4r4nBGvJM/9nFuXG4I4wH8DMPdso2x21UQBXhGI48nY=";
        })
        (fetchAlpinePackage {
          name = "libblkid-2.42.1-r0.apk";
          hash = "sha256-xaXaiqUYGWthOtRlRTW+X+IgAyU1NahOIm/OBV/pHho=";
        })
        (fetchAlpinePackage {
          name = "libeconf-0.8.3-r0.apk";
          hash = "sha256-KnkceVeWH4MM8xsE81+BSmOvz2M7rckFJCX/HPL2Q4A=";
        })
      ];

      qgaRoot =
        pkgs.runCommand "virtle-alpine-qga-root"
          {
            nativeBuildInputs = [ pkgs.libarchive ];
          }
          ''
            mkdir "$out"
            for package in ${pkgs.lib.escapeShellArgs qgaPackages}; do
              bsdtar --extract --file "$package" --directory "$out"
            done
            rm -f "$out"/.PKGINFO "$out"/.SIGN.*
          '';

      layeredImage = pkgs.dockerTools.buildLayeredImage {
        name = "virtle-alpine-rootfs";
        tag = "3.24.1";
        fromImage = alpine;
        architecture = "amd64";
        contents = [ qgaRoot ];
        extraCommands = ''
          mkdir -p etc run
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

        [[write_files]]
        guest_path = "/run/virtle-ready"
        text = "QEMU Guest Agent is ready\n"
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
