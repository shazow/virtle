{
  description = "virtle VM manager";

  inputs.nixpkgs.url = "nixpkgs";

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      release = import ./release.nix;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          virtle = pkgs.buildGoModule {
            pname = "virtle";
            inherit (release) version vendorHash;
            src = ./.;
            subPackages = [ "." ];
            # A Nix build has no VCS metadata for the Go toolchain to stamp, so
            # the version is passed in. The "v" matches how tag-built binaries
            # report themselves.
            ldflags = [ "-X main.version=v${release.version}" ];
            env.CGO_ENABLED = 0;
            meta.mainProgram = "virtle";
          };

          default = virtle;
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.virtle}/bin/virtle";
          meta.description = "Run virtle";
        };
      });

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          guestInit = pkgs.writeScript "virtle-integration-init" ''
            #!${pkgs.busybox}/bin/sh
            export PATH=${pkgs.busybox}/bin:${pkgs.qemu.ga}/bin
            mkdir -p /bin /dev /proc /sys /tmp /var/run
            mount -t devtmpfs devtmpfs /dev
            mount -t proc proc /proc
            mount -t sysfs sysfs /sys
            ln -s ${pkgs.busybox}/bin/sh /bin/sh
            ln -s ${pkgs.busybox}/bin/true /bin/true
            ln -s ${pkgs.busybox}/bin/false /bin/false
            while [ ! -e /dev/virtio-ports/org.qemu.guest_agent.0 ]; do
              ${pkgs.busybox}/bin/sleep 0.01
            done
            ${pkgs.qemu.ga}/bin/qemu-ga -m virtio-serial -p /dev/virtio-ports/org.qemu.guest_agent.0 &
            while true; do
              ${pkgs.busybox}/bin/sleep 3600
            done
          '';
          guestInitrd = pkgs.makeInitrd {
            name = "virtle-integration-initrd";
            contents = [
              {
                object = guestInit;
                symlink = "/init";
              }
            ];
          };
          guestKernel = "${pkgs.linuxPackages.kernel}/${
            if pkgs.stdenv.hostPlatform.isx86_64 then "bzImage" else "Image"
          }";
          guestMachine = if pkgs.stdenv.hostPlatform.isx86_64 then "microvm" else "virt";
          integrationTest = pkgs.buildGoModule {
            pname = "virtle-integration-test-binary";
            inherit (release) version vendorHash;
            src = ./.;
            subPackages = [
              "backend/qemu/internal/launch"
              "backend/qemu"
            ];
            tags = [ "integration" ];
            env.CGO_ENABLED = 0;
            buildTestBinaries = true;
          };
        in
        {
          # Runs the launch integration tests in a small VM where /bin/sh is
          # dash, covering the absolute guest shell path Virtle sends to QGA.
          integration = pkgs.vmTools.runInLinuxVM (
            pkgs.runCommand "virtle-integration-tests-${release.version}"
              {
                memSize = 1024;
                nativeBuildInputs = [ pkgs.qemu ];
              }
              ''
                ln -sfn ${pkgs.dash}/bin/dash /bin/sh
                test "$(readlink -f /bin/sh)" = ${pkgs.dash}/bin/dash
                ${integrationTest}/bin/launch.test -test.run '^TestIntegration' -test.v
                VIRTLE_INTEGRATION_KERNEL=${guestKernel} \
                  VIRTLE_INTEGRATION_INITRD=${guestInitrd}/initrd \
                  VIRTLE_INTEGRATION_MACHINE=${guestMachine} \
                  ${integrationTest}/bin/qemu.test -test.run '^TestIntegrationBackend$' -test.v
                touch $out
              ''
          );
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
