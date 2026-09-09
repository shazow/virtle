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
        // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          e2e-fast-fixture = import ./tests/e2e/fixtures/fast { inherit pkgs; };
          e2e-fast-userspace-fixture = import ./tests/e2e/fixtures/fast {
            inherit pkgs;
            userspace = true;
          };
          benchmark-backends = pkgs.writeShellApplication {
            name = "virtle-benchmark-backends";
            runtimeInputs = [ pkgs.python3 ];
            text = ''
              exec python ${./tests/e2e/run.py} \
                --virtle ${self.packages.${system}.virtle}/bin/virtle \
                --fixture ${self.packages.${system}.e2e-fast-fixture} "$@"
            '';
          };
        }
      );

      apps = forAllSystems (
        system:
        {
          default = {
            type = "app";
            program = "${self.packages.${system}.virtle}/bin/virtle";
            meta.description = "Run virtle";
          };
        }
        // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          benchmark-backends = {
            type = "app";
            program = "${self.packages.${system}.benchmark-backends}/bin/virtle-benchmark-backends";
            meta.description = "Directional Firecracker vs QEMU/KVM comparison";
          };
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          guestKernelPackage = pkgs.linuxPackages.kernel;
          firecrackerGuest = import ./docs/recipes/firecracker/guest.nix {
            inherit pkgs;
            virtle = self.packages.${system}.virtle;
          };
          guestCompressedModules = pkgs.makeModulesClosure {
            kernel = guestKernelPackage.modules;
            firmware = guestKernelPackage;
            rootModules = [
              "virtio_console"
              "virtio_mmio"
            ];
          };
          guestModules = pkgs.runCommand "virtle-integration-modules" { nativeBuildInputs = [ pkgs.xz ]; } ''
            mkdir -p $out
            cp -r ${guestCompressedModules}/lib $out/
            chmod -R u+w $out
            find $out -name '*.ko.xz' -exec unxz '{}' +
            sed -i 's/\.ko\.xz/\.ko/g' $out/lib/modules/${guestKernelPackage.modDirVersion}/modules.dep
            rm $out/lib/modules/${guestKernelPackage.modDirVersion}/modules.dep.bin
          '';
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
            modprobe virtio_mmio
            modprobe virtio_console
            guestAgentDevice=
            while [ -z "$guestAgentDevice" ]; do
              for namePath in /sys/class/virtio-ports/*/name; do
                if [ -e "$namePath" ] && [ "$(cat "$namePath")" = org.qemu.guest_agent.0 ]; then
                  guestAgentDevice=/dev/$(basename "$(dirname "$namePath")")
                  break
                fi
              done
              ${pkgs.busybox}/bin/sleep 0.01
            done
            exec ${pkgs.qemu.ga}/bin/qemu-ga -m virtio-serial -p "$guestAgentDevice"
          '';
          guestInitrd = pkgs.makeInitrd {
            name = "virtle-integration-initrd";
            contents = [
              {
                object = guestInit;
                symlink = "/init";
              }
              {
                object = guestModules;
                suffix = "/lib/modules";
                symlink = "/lib/modules";
              }
            ];
          };
          guestKernel = "${guestKernelPackage}/${
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
          e2e-runner =
            pkgs.runCommand "virtle-e2e-runner-tests"
              {
                nativeBuildInputs = [ pkgs.python3 ];
              }
              ''
                PYTHONDONTWRITEBYTECODE=1 python -m unittest discover -s ${./tests/e2e} -v
                touch $out
              '';
          # Runs the launch integration tests in a small VM where /bin/sh is
          # dash, covering the absolute guest shell path Virtle sends to QGA.
          integration = pkgs.vmTools.runInLinuxVM (
            pkgs.runCommand "virtle-integration-tests-${release.version}"
              {
                enableParallelBuilding = false;
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
        // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          e2e-fast =
            pkgs.runCommand "virtle-fast-e2e"
              {
                requiredSystemFeatures = [ "kvm" ];
                nativeBuildInputs = [ pkgs.python3 ];
              }
              ''
                output=$(mktemp -d)
                python ${./tests/e2e/run.py} \
                  --virtle ${self.packages.${system}.virtle}/bin/virtle \
                  --fixture ${self.packages.${system}.e2e-fast-fixture} \
                  --pairs 2 --warmup-pairs 0 \
                  --output "$output/results"
                touch $out
              '';
          # Firecracker requires real KVM; no TCG fallback and no skip-success.
          # SendCtrlAltDel (used to verify guest shutdown) is x86-only.
          firecracker =
            pkgs.runCommand "virtle-firecracker-e2e"
              {
                requiredSystemFeatures = [ "kvm" ];
                nativeBuildInputs = [ pkgs.python3 ];
              }
              ''
                test -r /dev/kvm && test -w /dev/kvm
                python ${./docs/recipes/firecracker/check.py} \
                  ${self.packages.${system}.virtle}/bin/virtle ${firecrackerGuest.manifest}
                touch $out
              '';
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
