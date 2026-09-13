{
  description = "Minimal Cloud Hypervisor guest booted and verified through virtle";
  inputs.virtle.url = "github:shazow/virtle";
  inputs.nixpkgs.follows = "virtle/nixpkgs";
  outputs =
    {
      self,
      virtle,
      nixpkgs,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      guest = import ./guest.nix {
        inherit pkgs;
        virtle = virtle.packages.${system}.default;
      };
    in
    {
      packages.${system} = {
        inherit (guest) rootfs initrd manifest;
        default = guest.run;
      };
      apps.${system}.default = {
        type = "app";
        program = "${guest.run}/bin/virtle-cloud-hypervisor";
      };
      formatter.${system} = pkgs.nixfmt;
    };
}
