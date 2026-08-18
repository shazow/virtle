// Package balloon implements the internal virtio-balloon feature.
//
// It owns QEMU argument lowering for the virtio-balloon device and the
// optional runtime controller that adjusts guest memory through QMP. The
// device configuration vocabulary (manifest.BalloonDevice) belongs to the
// manifest; this package consumes it.
package balloon
