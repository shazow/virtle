//go:build !linux

package vmm

import "github.com/shazow/virtle/backend/qemu/internal/launch"

type hostVSockCIDChecker struct{}

func newHostVSockCIDChecker() launch.VSockCIDChecker {
	return &hostVSockCIDChecker{}
}

func (c *hostVSockCIDChecker) Available(cid int) (bool, error) {
	return true, nil
}
