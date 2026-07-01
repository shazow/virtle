// Package balloon implements the internal virtio-balloon feature.
//
// It owns QEMU argument lowering for the virtio-balloon device and the
// optional runtime controller that adjusts guest memory through QMP, along
// with the configuration types shared by the manifest and manager.
package balloon
