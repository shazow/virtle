{
  description = "QEMU NixOS guest demonstrating virtle networking and secret injection";

  inputs.virtle.url = "github:shazow/virtle";
  inputs.nixpkgs.follows = "virtle/nixpkgs";

  outputs =
    {
      virtle,
      nixpkgs,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      guest = import ./guest.nix { inherit pkgs; };
      runner =
        check:
        pkgs.writeShellApplication {
          name = "virtle-networking${pkgs.lib.optionalString check "-check"}";
          runtimeInputs = with pkgs; [
            coreutils
            openssh
            qemu_kvm
            dnsmasq
            openssl
            python3
            iproute2
          ];
          text = ''
            exec python3 ${./.}/run.py \
              --virtle ${virtle.packages.${system}.default}/bin/virtle \
              --base-manifest ${guest.manifest} \
              --disk ${guest.diskImage}/root.qcow2 \
              ${pkgs.lib.optionalString check "--check"} "$@"
          '';
        };
      run = runner false;
      check = runner true;
    in
    {
      packages.${system} = {
        inherit (guest) diskImage manifest;
        inherit check;
        default = run;
      };
      apps.${system} = {
        default = {
          type = "app";
          program = "${run}/bin/virtle-networking";
        };
        check = {
          type = "app";
          program = "${check}/bin/virtle-networking-check";
        };
      };
      formatter.${system} = pkgs.nixfmt;
    };
}
