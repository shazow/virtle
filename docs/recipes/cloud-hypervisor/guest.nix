# Shared by the runnable recipe and the repository's actual KVM boot check.
{ pkgs, virtle }:
let
  kernelPackage = pkgs.linuxPackages.kernel;
  # Cloud Hypervisor boots the ELF kernel through its PVH entry point, which
  # the distribution kernel has, on x86_64 and the Image on aarch64.
  kernel =
    if pkgs.stdenv.hostPlatform.isx86_64 then
      "${kernelPackage.dev}/vmlinux"
    else
      "${kernelPackage}/Image";
  compressedModules = pkgs.makeModulesClosure {
    kernel = kernelPackage.modules;
    firmware = kernelPackage;
    # What the kernel builds in is recorded as such; the rest is copied.
    rootModules = [
      "virtio_pci"
      "virtio_blk"
      "ext4"
      "button"
      "evdev"
    ];
  };
  modules =
    pkgs.runCommand "virtle-cloud-hypervisor-modules"
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
    pkgs.runCommand "virtle-cloud-hypervisor-rootfs"
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
  ready = pkgs.writeScript "cloud-hypervisor-ready" ''
    #!${pkgs.busybox}/bin/sh
    set -eu
    export PATH=${pkgs.busybox}/bin
    # Drivers the distribution kernel builds as modules; the built-in ones
    # are simply there, and a missing one shows up at the mount below or
    # at shutdown.
    for module in virtio_pci virtio_blk ext4 button evdev; do
      modprobe "$module" 2>/dev/null || true
    done
    # Cloud Hypervisor's shutdown request is the ACPI power button: the
    # button driver reports it as a key press on an input device, and
    # BusyBox acpid runs the handler installed at /etc/acpi/PWRF/00000080.
    mkdir -p /var/run /mnt /tmp
    acpid -l /dev/null
    mount -t ext4 -o ro /dev/vda /mnt
    value=$(cat /mnt/input)
    result=$((value * 2))
    echo "$result" > /tmp/result
    test "$(cat /tmp/result)" = 42
    echo "VIRTLE_READY:$result"
  '';
  shutdown = pkgs.writeScript "cloud-hypervisor-shutdown" ''
    #!${pkgs.busybox}/bin/sh
    set -eu
    export PATH=${pkgs.busybox}/bin
    umount /mnt
    echo VIRTLE_SHUTDOWN:unmounted
    # A power-off ends Cloud Hypervisor; a reset restarts the guest in place.
    poweroff -f
  '';
  # acpid's handlers inherit its log for their output, so the marker is sent
  # to the console explicitly.
  powerButton = pkgs.writeScript "cloud-hypervisor-power-button" ''
    #!${pkgs.busybox}/bin/sh
    exec ${shutdown} >/dev/console 2>&1
  '';
  inittab = pkgs.writeText "cloud-hypervisor-inittab" ''
    ::sysinit:${ready}
  '';
  init = pkgs.writeScript "cloud-hypervisor-init" ''
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
    name = "virtle-cloud-hypervisor-initrd";
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
        object = powerButton;
        symlink = "/etc/acpi/PWRF/00000080";
      }
      {
        object = modules;
        suffix = "/lib/modules";
        symlink = "/lib/modules";
      }
    ];
  };
  manifest = pkgs.writeText "cloud-hypervisor-manifest.toml" ''
    backend = "cloud-hypervisor"
    [cloud-hypervisor]
    binary = "${pkgs.cloud-hypervisor}/bin/cloud-hypervisor"
    [machine]
    vcpu = 1
    memory = 256
    [kernel]
    path = "${kernel}"
    initrd_path = "${initrd}/initrd"
    serial = "print"
    params = ["rdinit=/init"]
    [[mounts]]
    type = "image"
    source = "${rootfs}/rootfs.ext4"
    read_only = true
    image.format = "raw"
  '';
  run = pkgs.writeShellApplication {
    name = "virtle-cloud-hypervisor";
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
