{
  description = "Minimal console-only NixOS guest for virtle";

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

      # Adapt systemdMinimal to NixOS stage 1 without adding full systemd.
      systemdInitrd = pkgs.symlinkJoin {
        name = "systemd-initrd-minimal";
        paths = [ pkgs.systemdMinimal ];
        postBuild = ''
          ln -s ${pkgs.busybox}/bin/false "$out/lib/systemd/systemd-bsod"

          # There are no real-root fstab mounts to discover.
          unit="$out/example/systemd/system/initrd-parse-etc.service"
          rm "$unit"
          sed \
            's|^ExecStart=.*|ExecStart=${pkgs.busybox}/bin/true|' \
            "${pkgs.systemdMinimal}/example/systemd/system/initrd-parse-etc.service" \
            > "$unit"
        '';
        passthru = pkgs.systemdMinimal.passthru;
      };

      nixos = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          "${nixpkgs}/nixos/modules/profiles/minimal.nix"
          "${nixpkgs}/nixos/modules/profiles/bashless.nix"
          (
            {
              config,
              lib,
              pkgs,
              ...
            }:
            {
              boot.loader.grub.enable = false;
              boot.hardwareScan = false;
              boot.initrd.includeDefaultModules = false;
              boot.initrd.availableKernelModules = [
                "ext4"
                "virtio_blk"
                "virtio_mmio"
              ];
              boot.initrd.kernelModules = [
                "ext4"
                "virtio_blk"
                "virtio_mmio"
              ];
              boot.initrd.systemd = {
                package = systemdInitrd;
                root = null;
                storePaths = [ pkgs.busybox ];
                suppressedUnits = [ "systemd-bsod.service" ];
                services.mount-root = {
                  description = "Mount the fixed virtio root disk";
                  requiredBy = [ "initrd-root-fs.target" ];
                  before = [
                    "initrd-find-etc.service"
                    "initrd-parse-etc.service"
                    "initrd-root-fs.target"
                    "sysroot-run.mount"
                  ];
                  after = [ "systemd-modules-load.service" ];
                  serviceConfig = {
                    ExecStartPre = "${pkgs.busybox}/bin/mkdir -p /sysroot";
                    ExecStart = "${pkgs.busybox}/bin/mount -t ext4 /dev/vda /sysroot";
                    RemainAfterExit = true;
                    Type = "oneshot";
                  };
                };
              };

              networking = {
                firewall.enable = false;
                hostName = "virtle";
                resolvconf.enable = false;
                useDHCP = false;
                useNetworkd = false;
              };

              nix.enable = false;
              nixpkgs.flake = {
                setFlakeRegistry = false;
                setNixPath = false;
              };

              i18n = {
                defaultLocale = "C.UTF-8";
                glibcLocales = null;
              };

              programs = {
                less.enable = lib.mkForce false;
                nano.enable = false;
              };
              security.pam.services = lib.mkForce { };
              security.sudo.enable = false;
              services = {
                dbus.enable = lib.mkForce false;
                fstrim.enable = false;
                getty.enable = false;
                logind.enable = false;
                lvm.enable = false;
                resolved.enable = false;
                timesyncd.enable = false;
                udev.enable = false;
              };

              environment = {
                defaultPackages = lib.mkForce [ ];
                systemPackages = lib.mkForce [ pkgs.busybox ];
              };
              fonts.fontconfig.enable = false;

              users.allowNoPasswordLogin = true;
              users.users.root = {
                ignoreShellProgramCheck = true;
                shell = "${pkgs.busybox}/bin/sh";
              };

              systemd.services.virtle-console = {
                description = "Virtle console shell";
                wantedBy = [ "virtle.target" ];
                unitConfig.DefaultDependencies = false;
                serviceConfig = {
                  ExecStart = "${pkgs.busybox}/bin/sh";
                  Restart = "always";
                  StandardInput = "tty-force";
                  StandardOutput = "tty";
                  StandardError = "tty";
                  TTYPath = "/dev/ttyS0";
                  TTYReset = true;
                  TTYVHangup = true;
                };
              };
              systemd.targets.virtle = {
                description = "Virtle console";
                unitConfig.DefaultDependencies = false;
              };

              systemd = {
                coredump.enable = false;
                network.enable = false;
                oomd.enable = false;
                package = pkgs.systemdMinimal;
                suppressedSystemUnits = [
                  "fstrim.service"
                  "systemd-fsck@.service"
                  "systemd-makefs@.service"
                  "systemd-mkswap@.service"
                  "uuidd.service"
                ];
              };
              system.fsPackages = lib.mkForce [ ];

              # Stage 1 already contains the only modules this appliance needs.
              # Do not retain the complete kernel module tree in the disk closure.
              system.systemBuilderCommands = lib.mkAfter ''
                rm "$out/kernel-modules"
                ln -s ${config.system.build.modulesClosure} "$out/kernel-modules"
              '';

              system.stateVersion = "25.11";
            }
          )
        ];
      };

      diskImage = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
        inherit pkgs;
        lib = nixpkgs.lib;
        config = nixos.config;
        additionalSpace = "16M";
        baseName = "root";
        copyChannel = false;
        format = "qcow2-compressed";
        installBootLoader = false;
        partitionTableType = "none";
      };

      kernel = "${nixos.config.system.build.kernel}/${nixos.config.system.boot.loader.kernelFile}";
      initrd = "${nixos.config.system.build.initialRamdisk}/${nixos.config.system.boot.loader.initrdFile}";

      manifest = pkgs.writeText "manifest.toml" ''
        networks = []

        [machine]
        memory = 512
        vcpu = 8

        [kernel]
        path = "${kernel}"
        initrd_path = "${initrd}"
        serial = "console"
        params = [
          "init=${nixos.config.system.build.toplevel}/init",
          "loglevel=3",
          "quiet",
          "rd.systemd.show_status=false",
          "systemd.show_status=false",
          "systemd.unit=virtle.target",
        ]

        [[mounts]]
        type = "image"
        source = "root.qcow2"
        image.format = "qcow2"
      '';

      runVirtle = pkgs.writeShellApplication {
        name = "virtle-nixos-minimal";
        runtimeInputs = with pkgs; [
          coreutils
          qemu_kvm
        ];
        text = ''
          work_dir="$(mktemp -d "''${TMPDIR:-/tmp}/virtle-nixos-minimal.XXXXXX")"
          trap 'rm -rf -- "$work_dir"' EXIT

          cp "${manifest}" "$work_dir/manifest.toml"
          qemu-img create -q -f qcow2 -F qcow2 \
            -b "${diskImage}/root.qcow2" \
            "$work_dir/root.qcow2"

          cd "$work_dir"
          "${virtlePackage}/bin/virtle" launch -v
        '';
      };
    in
    {
      apps.${system}.default = {
        type = "app";
        program = "${runVirtle}/bin/virtle-nixos-minimal";
        meta.description = "Launch the minimal NixOS guest on virtle's console";
      };

      packages.${system} = {
        default = diskImage;
        inherit diskImage;
        toplevel = nixos.config.system.build.toplevel;
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
