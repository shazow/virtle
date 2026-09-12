{ pkgs }:
let
  demoCheck = pkgs.writeShellApplication {
    name = "demo-check";
    runtimeInputs = with pkgs; [
      curl
      bind.dnsutils
      coreutils
      gnugrep
    ];
    text = ''exec ${pkgs.bash}/bin/bash ${./demo-check.sh} "$@"'';
  };

  nixos = import "${pkgs.path}/nixos/lib/eval-config.nix" {
    inherit pkgs;
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      (
        { lib, ... }:
        {
          boot.loader.grub.enable = false;
          boot.initrd.availableKernelModules = [
            "virtio_blk"
            "virtio_mmio"
            "virtio_pci"
          ];
          boot.kernelModules = [
            "virtio_net"
            "virtio_console"
            "vmw_vsock_virtio_transport"
          ];
          fileSystems."/" = {
            device = "/dev/vda";
            fsType = "ext4";
          };

          networking = {
            hostName = "virtle-networking";
            useDHCP = false;
            useNetworkd = true;
          };
          systemd.network = {
            enable = true;
            wait-online.anyInterface = true;
            networks."10-virtle" = {
              matchConfig.Name = "en* eth*";
              networkConfig.DHCP = "ipv4";
              linkConfig.RequiredForOnline = "routable";
              dhcpV4Config.UseDNS = true;
            };
          };
          services.resolved = {
            enable = true;
            settings.Resolve.FallbackDNS = "";
          };

          services.openssh = {
            enable = true;
            settings = {
              KbdInteractiveAuthentication = false;
              PasswordAuthentication = false;
              PermitRootLogin = "prohibit-password";
            };
          };
          systemd.services.sshd = {
            wants = [ "network-online.target" ];
            after = [ "network-online.target" ];
          };
          services.qemuGuest.enable = true;
          systemd.services.qemu-guest-agent.path = with pkgs; [
            bash
            coreutils
            gnugrep
          ];

          nix.enable = false;
          nixpkgs.flake = {
            setFlakeRegistry = false;
            setNixPath = false;
          };
          documentation.enable = false;
          environment.defaultPackages = lib.mkForce [ ];
          environment.systemPackages = with pkgs; [
            bashInteractive
            coreutils
            gnugrep
            curl
            bind.dnsutils
            demoCheck
          ];
          environment.interactiveShellInit = ''
            if [ -r /etc/virtle/demo.env ]; then
              . /etc/virtle/demo.env
            fi
          '';
          system.stateVersion = "25.11";
        }
      )
    ];
  };

  diskImage = import "${pkgs.path}/nixos/lib/make-disk-image.nix" {
    inherit pkgs;
    inherit (pkgs) lib;
    config = nixos.config;
    additionalSpace = "256M";
    baseName = "root";
    copyChannel = false;
    format = "qcow2-compressed";
    installBootLoader = false;
    partitionTableType = "none";
  };
  kernel = "${nixos.config.system.build.kernel}/${nixos.config.system.boot.loader.kernelFile}";
  initrd = "${nixos.config.system.build.initialRamdisk}/${nixos.config.system.boot.loader.initrdFile}";
  init = "${nixos.config.system.build.toplevel}/init";
  # Keep vsock transport independent of the host's SSH configuration. These
  # disposable guests have a new host key and an ephemeral CID on every run.
  sshConfig = pkgs.writeText "virtle-networking-ssh.conf" ''
    Host vsock/*
      ProxyCommand ${pkgs.systemd}/lib/systemd/systemd-ssh-proxy %h %p
      ProxyUseFdpass yes
      CheckHostIP no
      StrictHostKeyChecking no
      UserKnownHostsFile /dev/null
      BatchMode yes
  '';
  manifest = pkgs.writeText "virtle-networking-base.toml" ''
    backend = "qemu"
    host_name = "virtle-networking"

    [machine]
    vcpu = 1
    memory = 768

    [kernel]
    path = "${kernel}"
    initrd_path = "${initrd}"
    serial = "print"
    params = ["root=/dev/vda", "rw", "init=${init}", "systemd.ssh_listen=vsock::22"]

    [qemu]
    exec = ["${pkgs.qemu_kvm}/bin/qemu-system-x86_64"]

    [ssh]
    exec = ["${pkgs.openssh}/bin/ssh", "-F", "${sshConfig}"]
    user = "root"
    autoprovision = true

    [[mounts]]
    type = "image"
    source = "root.qcow2"
    image.format = "qcow2"
  '';
in
{
  inherit
    diskImage
    kernel
    initrd
    init
    manifest
    ;
}
