{
  description = "virtle VM manager";

  inputs.nixpkgs.url = "nixpkgs";

  outputs =
    { self, nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system} = rec {
        virtle = pkgs.buildGoModule {
          pname = "virtle";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-r9IW9NAU67IVMDkeznVqohw2mY9P3I9m+TkLFOJAdGc=";
          subPackages = [ "." ];
          env.CGO_ENABLED = 0;
          meta.mainProgram = "virtle";
        };

        default = virtle;
      };

      apps.${system} = {
        default = {
          type = "app";
          program = "${self.packages.${system}.virtle}/bin/virtle";
          meta.description = "Run virtle";
        };
      };

      formatter.${system} = pkgs.nixfmt;
    };
}
