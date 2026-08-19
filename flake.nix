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
          integrationTest = pkgs.buildGoModule {
            pname = "virtle-integration-test-binary";
            inherit (release) version vendorHash;
            src = ./.;
            subPackages = [ "backend/qemu/internal/launch" ];
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
                memSize = 512;
              }
              ''
                ln -sfn ${pkgs.dash}/bin/dash /bin/sh
                test "$(readlink -f /bin/sh)" = ${pkgs.dash}/bin/dash
                ${integrationTest}/bin/launch.test -test.run '^TestIntegration' -test.v
                touch $out
              ''
          );
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
