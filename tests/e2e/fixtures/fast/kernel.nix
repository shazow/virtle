# One kernel build for both loaders: ELF vmlinux (Firecracker), bzImage (QEMU).
{ pkgs }:
let
  configured = pkgs.linux.override {
    defconfig = "tinyconfig";
    enableCommonConfig = false;
    autoModules = false;
    kernelPatches = [ ];
    structuredExtraConfig = with pkgs.lib.kernel; {
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
      PCI = no;
      ACPI = no;
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
