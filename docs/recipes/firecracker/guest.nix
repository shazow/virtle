# Shared by the runnable recipe and the repository's actual KVM boot check.
{ pkgs, virtle }:
let
  kernelPackage = pkgs.linuxPackages.kernel;
  kernel =
    if pkgs.stdenv.hostPlatform.isx86_64 then
      "${kernelPackage.dev}/vmlinux"
    else
      "${kernelPackage}/Image";
  compressedModules = pkgs.makeModulesClosure {
    kernel = kernelPackage.modules;
    firmware = kernelPackage;
    rootModules = [
      "virtio_mmio"
      "virtio_blk"
      "ext4"
      "i8042"
      "atkbd"
    ];
  };
  modules =
    pkgs.runCommand "virtle-firecracker-modules"
      {
        nativeBuildInputs = [
          pkgs.xz
          pkgs.zstd
        ];
      }
      ''
        mkdir -p $out
        cp -r ${compressedModules}/lib $out/
        chmod -R u+w $out
        find $out -name '*.ko.xz' -exec unxz '{}' +
        find $out -name '*.ko.zst' -exec unzstd --rm '{}' +
        sed -i -e 's/\.ko\.xz/\.ko/g' -e 's/\.ko\.zst/\.ko/g' $out/lib/modules/${kernelPackage.modDirVersion}/modules.dep
        rm -f $out/lib/modules/${kernelPackage.modDirVersion}/modules.dep.bin
      '';
  rootfs =
    pkgs.runCommand "virtle-firecracker-rootfs"
      {
        nativeBuildInputs = [
          pkgs.e2fsprogs
          pkgs.fakeroot
        ];
      }
      ''
        mkdir -p root $out
        echo 21 > root/input
        chmod 0755 root
        chmod 0644 root/input
        touch -d @1 root root/input
        truncate -s 16M $out/rootfs.ext4
        # chown and mke2fs must share a fakeroot session: -d imports source owners.
        fakeroot -- sh -eu <<'EOF'
        chown -R 0:0 root
        E2FSPROGS_FAKE_TIME=1 mke2fs -q -t ext4 -F -U 00000000-0000-0000-0000-000000000042 \
          -E root_owner=0:0,hash_seed=00000000-0000-0000-0000-000000000042,lazy_itable_init=0,lazy_journal_init=0 \
          -d root $out/rootfs.ext4
        EOF
        # Inspect actual image inodes, including the file imported by mke2fs -d.
        for inode in / /lost+found /input; do
          debugfs -R "stat $inode" $out/rootfs.ext4 | awk '
            /User:/ { seen = 1; if ($2 != 0 || $4 != 0) { print "non-root inode:", $0; exit 1 } }
            END { if (!seen) exit 1 }
          '
        done
      '';
  ready = pkgs.writeScript "firecracker-ready" ''
    #!${pkgs.busybox}/bin/sh
    set -eu
    export PATH=${pkgs.busybox}/bin
    modprobe virtio_mmio
    modprobe virtio_blk
    modprobe ext4
    # Firecracker's x86 shutdown action injects keyboard Ctrl-Alt-Del.
    # BusyBox modprobe does not import module options from /proc/cmdline.
    modprobe i8042 noaux=1 nomux=1 dumbkbd=1
    modprobe atkbd
    mkdir -p /mnt /tmp
    mount -t ext4 -o ro /dev/vda /mnt
    value=$(cat /mnt/input)
    result=$((value * 2))
    echo "$result" > /tmp/result
    test "$(cat /tmp/result)" = 42
    echo "VIRTLE_READY:$result"
  '';
  shutdown = pkgs.writeScript "firecracker-shutdown" ''
    #!${pkgs.busybox}/bin/sh
    set -eu
    export PATH=${pkgs.busybox}/bin
    umount /mnt
    echo VIRTLE_SHUTDOWN:unmounted
    # x86 Firecracker exits on an i8042 reset; poweroff leaves the VMM alive.
    reboot -f
  '';
  inittab = pkgs.writeText "firecracker-inittab" ''
    ::sysinit:${ready}
    ::ctrlaltdel:${shutdown}
  '';
  init = pkgs.writeScript "firecracker-init" ''
    #!${pkgs.busybox}/bin/sh
    export PATH=${pkgs.busybox}/bin
    mkdir -p /dev /proc /sys /bin
    mount -t devtmpfs devtmpfs /dev
    mount -t proc proc /proc
    mount -t sysfs sysfs /sys
    ln -s ${pkgs.busybox}/bin/sh /bin/sh
    exec ${pkgs.busybox}/bin/init
  '';
  initrd = pkgs.makeInitrd {
    name = "virtle-firecracker-initrd";
    contents = [
      {
        object = init;
        symlink = "/init";
      }
      {
        object = inittab;
        symlink = "/etc/inittab";
      }
      {
        object = modules;
        suffix = "/lib/modules";
        symlink = "/lib/modules";
      }
    ];
  };
  manifest = pkgs.writeText "firecracker-manifest.toml" ''
    backend = "firecracker"
    [firecracker]
    binary = "${pkgs.firecracker}/bin/firecracker"
    [machine]
    vcpu = 1
    memory = 256
    [kernel]
    path = "${kernel}"
    initrd_path = "${initrd}/initrd"
    serial = "print"
    params = ["console=ttyS0", "reboot=k", "panic=-1", "pci=off", "rdinit=/init", "i8042.noaux", "i8042.nomux", "i8042.dumbkbd"]
    [[mounts]]
    type = "image"
    source = "${rootfs}/rootfs.ext4"
    read_only = true
    image.format = "raw"
  '';
  run = pkgs.writeShellApplication {
    name = "virtle-firecracker";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      work_dir=$(mktemp -d)
      trap 'rm -rf -- "$work_dir"' EXIT
      cd "$work_dir"
      ${pkgs.python3}/bin/python ${./check.py} ${virtle}/bin/virtle ${manifest}
    '';
  };
in
{
  inherit
    kernel
    initrd
    rootfs
    manifest
    run
    ;
}
