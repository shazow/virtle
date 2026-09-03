// Package launch contains the host-side building blocks shared by the QEMU
// backend's launch flow: launch planning, the runtime lock and launch pid
// file, suspend-state persistence and resume validation, socket, QMP, and
// guest-agent readiness waits, guest file provisioning and write-back, the
// SSH session retry loop, managed process teardown, and launch timing stats.
// It is consumed by backend/qemu/internal/vmm and backend/qemu/session.
package launch
