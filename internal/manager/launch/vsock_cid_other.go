//go:build !linux

package launch

type hostVSockCIDChecker struct{}

// NewHostVSockCIDChecker reports which vsock CIDs the host has free.
func NewHostVSockCIDChecker() VSockCIDChecker {
	return &hostVSockCIDChecker{}
}

func (c *hostVSockCIDChecker) Available(cid int) (bool, error) {
	return true, nil
}
