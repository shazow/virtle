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

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
