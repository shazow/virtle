# virtle 🐢🪐
VM manager for sandbox workflows.

**Status**: Migrating `virtie` from https://github.com/shazow/agentspace to this standalone project.

## Features

- Runs a QEMU microvm.
- Manages [`virtiofsd`](https://gitlab.com/virtio-fs/virtiofsd) daemons for virtiofs mounts.
- Provisions SSH between host and guest.
- Connects over SSH upon boot via signaling.
- Write files between guest and host on boot or shutdown.
- Suspend and resume.
- Notification execution hooks.
- ... and much more!

## License

MIT
