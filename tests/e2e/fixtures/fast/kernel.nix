# One kernel build for three loaders: ELF vmlinux (Firecracker, and Cloud
# Hypervisor through its PVH entry point), bzImage (QEMU).
{
  pkgs,
  userspace ? false,
}:
let
  configured = pkgs.linux.override {
    defconfig = "tinyconfig";
    enableCommonConfig = false;
    autoModules = false;
    kernelPatches = [ ];
    structuredExtraConfig =
      with pkgs.lib.kernel;
      {
        "64BIT" = yes;
        # tinyconfig defaults to XZ; favor fast decompression for QEMU's loader.
        KERNEL_XZ = no;
        KERNEL_GZIP = yes;
        SMP = yes;
        NR_CPUS = freeform "8";
        HYPERVISOR_GUEST = yes;
        PARAVIRT = yes;
        KVM_GUEST = yes;
        X86_MPPARSE = yes;
        HIGH_RES_TIMERS = yes;
        PRINTK = yes;
        MULTIUSER = yes;
        FUTEX = yes;
        EPOLL = yes;
        SIGNALFD = yes;
        BINFMT_ELF = yes;
        BINFMT_SCRIPT = yes;
        COREDUMP = no;
        MODULES = no;
        # Cloud Hypervisor's devices are virtio-PCI, enumerated through
        # ACPI, and its shutdown request is the ACPI power button; the tiny
        # power button driver turns that into SIGUSR2 for init (BusyBox
        # init's power-off), so the guest runs no acpid. The MMIO loaders
        # boot with pci=off acpi=off and skip all of this.
        PCI = yes;
        PCI_MSI = yes;
        ACPI = yes;
        ACPI_BUTTON = no;
        ACPI_TINY_POWER_BUTTON = yes;
        ACPI_TINY_POWER_BUTTON_SIGNAL = freeform "12";
        # Nothing here has a battery, a fan, or a thermal zone worth a
        # driver. (ACPI_PROCESSOR stays on: the build keeps it whatever
        # this says.)
        ACPI_AC = option no;
        ACPI_BATTERY = option no;
        ACPI_FAN = option no;
        ACPI_THERMAL = option no;
        ACPI_TABLE_UPGRADE = option no;
        PVH = yes;
        VIRTIO_PCI = yes;
        VIRTIO_PCI_LEGACY = no;
        # virtio-fs shares on Cloud Hypervisor and QEMU.
        FUSE_FS = yes;
        VIRTIO_FS = yes;
        BLK_DEV_INITRD = yes;
        RD_GZIP = yes;
        BLOCK = yes;
        BLK_DEV = yes;
        VIRTIO_MENU = yes;
        VIRTIO_MMIO = yes;
        VIRTIO_MMIO_CMDLINE_DEVICES = yes;
        VIRTIO_BLK = yes;
        VIRTIO_CONSOLE = yes;
        EXT4_FS = yes;
        DEVTMPFS = yes;
        PROC_FS = yes;
        SYSFS = yes;
        SHMEM = yes;
        TMPFS = yes;
        TTY = yes;
        SERIAL_8250 = yes;
        SERIAL_8250_CONSOLE = yes;
        SERIAL_8250_NR_UARTS = freeform "1";
        SERIAL_8250_RUNTIME_UARTS = freeform "1";
        # Firecracker's x86 graceful shutdown injects keyboard Ctrl-Alt-Del.
        INPUT = yes;
        INPUT_KEYBOARD = yes;
        KEYBOARD_ATKBD = yes;
        SERIO = yes;
        SERIO_I8042 = yes;
        # A virtio-net NIC with IPv4 only: enough for a DHCP lease, a TCP
        # echo, and name lookups over the virtle network. The unrelated NET
        # defaults stay out.
        NET = yes;
        INET = yes;
        IPV6 = no;
        PACKET = yes;
        NETDEVICES = yes;
        NET_CORE = yes;
        VIRTIO_NET = yes;
        BQL = no;
        ETHTOOL_NETLINK = no;
        NETWORK_FILESYSTEMS = no;
        NET_FLOW_LIMIT = no;
        RFS_ACCEL = no;
        WIRELESS = no;
        # NETDEVICES defaults these on; WLAN would select WIRELESS back in.
        WLAN = no;
        ETHERNET = no;
      }
      // pkgs.lib.optionalAttrs userspace {
        # Common userspace primitives required by Go, libuv and similar tools.
        EVENTFD = yes;
        INOTIFY_USER = yes;
        FILE_LOCKING = yes;
        UNIX = yes;
        AF_UNIX_OOB = no;
      };
  };
in
# The generic builder assumes MODULES=y when installing development outputs.
# The manual builder supports a monolithic kernel without a module closure.
(pkgs.linuxManualConfig {
  inherit (configured) src version configfile;
  config = {
    CONFIG_MODULES = "n";
    CONFIG_FW_LOADER = "n";
  };
}).overrideAttrs
  {
    postInstall = ''
      cp vmlinux $out/vmlinux
    '';
  }
