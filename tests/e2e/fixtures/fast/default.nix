{
  pkgs,
  workload ? ./ready,
  userspace ? false,
}:
let
  kernel = import ./kernel.nix { inherit pkgs userspace; };
  busybox = pkgs.pkgsStatic.busybox;
  # One guest tree, packed two ways: as the initramfs and as a raw ext4 root
  # disk. Applets are linked at build time so a read-only root works.
  guestTree = pkgs.runCommand "virtle-fast-guest-tree" { } ''
    mkdir -p $out/{bin,dev,etc,mnt,proc,sys,tmp}
    cp ${busybox}/bin/busybox $out/bin/busybox
    for applet in $($out/bin/busybox --list); do
      [ "$applet" = busybox ] || ln -sf busybox $out/bin/$applet
    done
    cp ${./init} $out/init
    cp ${workload} $out/bin/ready
    cp ${./shutdown} $out/bin/shutdown
    cp ${./dhcp-bound} $out/bin/dhcp-bound
    chmod +x $out/init $out/bin/{ready,shutdown,dhcp-bound}
    cp ${./inittab} $out/etc/inittab
    # The lease script writes the resolver under /tmp; the root may be read-only.
    ln -s /tmp/resolv.conf $out/etc/resolv.conf
    echo 21 > $out/input
    find $out -exec touch -h -d @1 '{}' +
  '';
  initrd =
    pkgs.runCommand "virtle-fast-initrd"
      {
        nativeBuildInputs = [
          pkgs.cpio
          pkgs.gzip
        ];
      }
      ''
        mkdir -p $out
        # Stable order, metadata and gzip header; no Nix store closure in the guest.
        (cd ${guestTree}; find . -print0 | LC_ALL=C sort -z | \
          cpio --null -o --format=newc --owner=0:0 --reproducible | gzip -n -1 > $out/initrd)
      '';
  rootfs =
    pkgs.runCommand "virtle-fast-rootfs"
      {
        nativeBuildInputs = [
          pkgs.e2fsprogs
          pkgs.fakeroot
        ];
      }
      ''
        mkdir -p $out
        cp -a ${guestTree} root
        chmod -R u+w root
        truncate -s 16M $out/rootfs.ext4
        # chown and mke2fs must share a fakeroot session: -d imports source owners.
        fakeroot -- sh -eu <<'EOF'
        chown -R 0:0 root
        E2FSPROGS_FAKE_TIME=1 mke2fs -q -t ext4 -F -U 00000000-0000-0000-0000-000000000042 \
          -E root_owner=0:0,hash_seed=00000000-0000-0000-0000-000000000042,lazy_itable_init=0,lazy_journal_init=0 \
          -d root $out/rootfs.ext4
        EOF
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
    params = ["pci=off", "rdinit=/init", "quiet", "i8042.noaux", "i8042.nomux", "i8042.dumbkbd", "i8042.nopnp"]
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
        rootfs
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
    ln -s ${rootfs}/rootfs.ext4 $out/rootfs.ext4
    ln -s ${metadata} $out/fixture.json
  ''
