package qemu

// Link selects how the guest NIC's frames reach the host. It is sealed
// (unexported method): User, TAP, or Stream. Nil means User without a
// Backend.Network and Stream with one; a pair that cannot work fails Start
// with an error wrapping errors.ErrUnsupported.
type Link interface{ link() }

// User is QEMU's built-in user networking (slirp): NAT and port forwards
// inside QEMU, with no host privilege and no host-side visibility. It is
// the default without a Backend.Network and cannot attach to one.
type User struct{}

// TAP is a host TAP device that already exists in virtle's network
// namespace (an operator's ip tuntap); the host kernel provides the
// network, and the operator owns bridging, NAT, and forwards. It cannot
// attach to a Backend.Network, and Spec.Ports are rejected.
type TAP struct {
	Name string // the interface name, as ip link shows it
}

// Stream carries frames to Backend.Network over a socket QEMU inherits
// (-netdev stream). It is the default with a Network and needs one.
type Stream struct{}

func (User) link()   {}
func (TAP) link()    {}
func (Stream) link() {}
