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

      # Disabling cgo links the binary statically, so it carries no .dynamic
      # section: patchelf's RPATH shrink and the $TMPDIR reference audit have
      # nothing to inspect and only print "cannot find section '.dynamic'"
      # during fixupPhase. Drop the two fixup opt-outs if cgo is ever enabled.
      staticGo = {
        env.CGO_ENABLED = 0;
        dontPatchELF = true;
        noAuditTmpdir = true;
      };
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          virtle = pkgs.buildGoModule (
            staticGo
            // {
              pname = "virtle";
              inherit (release) version vendorHash;
              src = ./.;
              subPackages = [ "." ];
              meta.mainProgram = "virtle";
            }
          );

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
          integration = pkgs.buildGoModule (
            staticGo
            // {
              pname = "virtle-integration-tests";
              inherit (release) version vendorHash;
              src = ./.;
              tags = [ "integration" ];
              nativeCheckInputs = [ pkgs.dash ];
            }
          );
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
