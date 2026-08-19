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
        in
        {
          # Runs the full Go test suite with the "integration" build tag in an
          # environment that guarantees the tools the integration tests
          # exercise (dash for the POSIX guest directory install script).
          integration = pkgs.buildGoModule {
            pname = "virtle-integration-tests";
            inherit (release) version vendorHash;
            src = ./.;
            tags = [ "integration" ];
            env.CGO_ENABLED = 0;
            nativeCheckInputs = [ pkgs.dash ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
