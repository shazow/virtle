package cloudhypervisor

// Link selects how the guest NIC's frames reach the host. It is sealed
// (unexported method); TAP is the only path Cloud Hypervisor offers without
// a vhost-user helper: the VMM opens a host TAP device, so frames go to the
// host kernel, never to a network virtle runs. Nil means no NIC.
type Link interface{ link() }

// TAP is a host TAP device that already exists in virtle's network
// namespace (an operator's ip tuntap) or that Cloud Hypervisor may create,
// which needs CAP_NET_ADMIN; the host kernel provides the network, and the
// operator owns bridging, NAT, and forwards, so Spec.Ports stay rejected.
type TAP struct {
	Name string // the interface name, as ip link shows it
}

func (TAP) link() {}
