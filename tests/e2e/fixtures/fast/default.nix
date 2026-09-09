{
  pkgs,
  workload ? ./ready,
  userspace ? false,
}:
let
  kernel = import ./kernel.nix { inherit pkgs userspace; };
  busybox = pkgs.pkgsStatic.busybox;
  initrd =
    pkgs.runCommand "virtle-fast-initrd"
      {
        nativeBuildInputs = [
          pkgs.cpio
          pkgs.gzip
        ];
      }
      ''
        mkdir -p root/{bin,dev,etc,proc,sys,tmp} $out
        cp ${busybox}/bin/busybox root/bin/busybox
        ln -s busybox root/bin/sh
        cp ${./init} root/init
        cp ${workload} root/bin/ready
        cp ${./shutdown} root/bin/shutdown
        chmod +x root/init root/bin/{ready,shutdown}
        cp ${./inittab} root/etc/inittab
        echo 21 > root/input
        # Stable order, metadata and gzip header; no Nix store closure in the guest.
        find root -exec touch -h -d @1 '{}' +
        (cd root; find . -print0 | LC_ALL=C sort -z | \
          cpio --null -o --format=newc --owner=0:0 --reproducible | gzip -n -1 > $out/initrd)
      '';
  common = isQemu: ''
    networks = []
    [machine]
    vcpu = 1
    memory = 128
    ${pkgs.lib.optionalString isQemu ''
      type = "microvm"
      kvm = true
    ''}
    [kernel]
    initrd_path = "${initrd}/initrd"
    serial = "print"
    params = ["console=ttyS0", "reboot=k", "panic=-1", "pci=off", "rdinit=/init", "quiet", "i8042.noaux", "i8042.nomux", "i8042.dumbkbd", "i8042.nopnp"]
  '';
  firecracker = pkgs.writeText "virtle-fast-firecracker.toml" ''
    backend = "firecracker"
    ${common false}
    path = "${kernel}/vmlinux"
    [firecracker]
    binary = "${pkgs.firecracker}/bin/firecracker"
  '';
  qemu = pkgs.writeText "virtle-fast-qemu.toml" ''
    backend = "qemu"
    ${common true}
    path = "${kernel}/bzImage"
    [qemu]
    exec = ["${pkgs.qemu_kvm}/bin/qemu-system-x86_64"]
    machine_options = { accel = "kvm", acpi = "off", pcie = "off", pit = "off", pic = "off", rtc = "off", usb = "off", x-option-roms = "off" }
    [vsock]
    enabled = false
  '';
  metadata = pkgs.writeText "virtle-fast-fixture.json" (
    builtins.toJSON {
      kernel_profile = if userspace then "userspace" else "minimal";
      kernel_version = kernel.version;
      busybox_version = busybox.version;
      firecracker_version = pkgs.firecracker.version;
      qemu_version = pkgs.qemu_kvm.version;
    }
  );
in
pkgs.runCommand "virtle-fast-fixture"
  {
    passthru = {
      inherit
        kernel
        initrd
        firecracker
        qemu
        ;
    };
  }
  ''
    mkdir -p $out
    ln -s ${firecracker} $out/firecracker.toml
    ln -s ${qemu} $out/qemu.toml
    ln -s ${kernel}/vmlinux $out/vmlinux
    ln -s ${kernel}/bzImage $out/bzImage
    ln -s ${kernel.configfile} $out/kernel.config
    ln -s ${initrd}/initrd $out/initrd
    ln -s ${metadata} $out/fixture.json
  ''
