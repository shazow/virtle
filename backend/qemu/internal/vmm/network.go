package vmm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// defaultNetworkMTU is what a network that does not report an MTU gets.
const defaultNetworkMTU = 1500

// networkAttachment is one launch's guest NIC on a vmnet.Network: the port,
// QEMU's end of the frame socket until QEMU has inherited it, and the
// forwards exposed through the port, keyed so Detach can find them.
type networkAttachment struct {
	id   string
	port vmnet.Port
	mtu  int

	mu        sync.Mutex
	guestEnd  *os.File
	exposures map[forwardKey]io.Closer
	closed    bool
}

type forwardKey struct {
	proto       vm.Proto
	host, guest string
}

func keyFor(f vm.Forward) forwardKey {
	return forwardKey{proto: cmp.Or(f.Proto, vm.TCP), host: f.HostAddr, guest: f.GuestAddr}
}

// managedNetdev is what the attached NIC contributes to QEMU's arguments:
// its socket is the FD-th inherited file.
type managedNetdev struct {
	ID  string
	FD  int
	MAC string
	MTU int
}

// managedNetDevice finds the manifest NIC that attaches to a vmnet.Network.
func managedNetDevice(mf *manifest.Manifest) *manifest.QEMUNetDevice {
	for i := range mf.QEMU.Devices.Network {
		if mf.QEMU.Devices.Network[i].Managed {
			return &mf.QEMU.Devices.Network[i]
		}
	}
	return nil
}

// attachNetwork attaches the manifest's virtle NIC, if it has one, before
// QEMU is launched, so the port's MAC lands on the command line and its
// address is known before the guest boots. A resumed machine attaches with
// the identity it was suspended with.
func (m *manager) attachNetwork(ctx context.Context, plan *launch.Plan) (*networkAttachment, error) {
	dev := managedNetDevice(plan.Manifest)
	var checkpoint *vmnet.NetworkState
	if plan.ResumeState != nil {
		checkpoint = plan.ResumeState.NetworkState
	}
	if dev == nil {
		if checkpoint != nil {
			return nil, fmt.Errorf("saved network state requires a virtle network device: %w", errors.ErrUnsupported)
		}
		return nil, nil
	}
	if m.network == nil {
		return nil, fmt.Errorf("network %q has type virtle, which needs qemu.Backend.Network: %w", dev.ID, errors.ErrUnsupported)
	}
	opts := vmnet.AttachOptions{Name: plan.Manifest.Identity.HostName, Egress: plan.Options.Egress}
	if dev.MacAddress != "" {
		mac, err := net.ParseMAC(dev.MacAddress)
		if err != nil {
			return nil, fmt.Errorf("network %q: %w", dev.ID, err)
		}
		opts.MAC = mac
	}
	if state := plan.ResumeState; state != nil && state.NetworkMAC != "" {
		mac, err := net.ParseMAC(state.NetworkMAC)
		if err != nil {
			return nil, fmt.Errorf("saved network identity: %w", err)
		}
		addr, err := netip.ParseAddr(state.NetworkAddr)
		if err != nil {
			return nil, fmt.Errorf("saved network identity: %w", err)
		}
		opts.MAC, opts.Addr = mac, addr
	}
	if checkpoint != nil {
		network, ok := m.network.(vmnet.StatefulNetwork)
		if !ok {
			return nil, fmt.Errorf("network %q cannot restore saved network state: %w", dev.ID, errors.ErrUnsupported)
		}
		if err := network.RestoreNetworkState(*checkpoint); err != nil {
			return nil, fmt.Errorf("restore network %q state: %w", dev.ID, err)
		}
	}
	mtu := networkMTU(m.network)
	hostEnd, guestEnd, err := framePair()
	if err != nil {
		return nil, fmt.Errorf("network %q: %w", dev.ID, err)
	}
	port, err := m.network.Attach(ctx, vmnet.QEMUStream(hostEnd, mtu), opts)
	if err != nil {
		_ = hostEnd.Close()
		_ = guestEnd.Close()
		return nil, fmt.Errorf("attach network %q: %w", dev.ID, err)
	}
	a := &networkAttachment{id: dev.ID, port: port, mtu: mtu, guestEnd: guestEnd, exposures: make(map[forwardKey]io.Closer)}
	for _, f := range dev.Forward {
		forward := vm.Forward{Proto: vm.Proto(f.Proto), HostAddr: f.Host, GuestAddr: f.Guest}
		if err := a.expose(ctx, forward); err != nil {
			return nil, errors.Join(err, a.Close())
		}
	}
	if m.logger != nil {
		m.logger.Info("attached network", "id", dev.ID, "addr", port.Addr(), "mac", port.MAC().String())
	}
	return a, nil
}

// networkMTU asks the network for its MTU when it reports one; the guest is
// told the same through host_mtu.
func networkMTU(n vmnet.Network) int {
	if r, ok := n.(interface{ MTU() int }); ok && r.MTU() > 0 {
		return r.MTU()
	}
	return defaultNetworkMTU
}

// framePair makes the socket QEMU's stream netdev shares with the network:
// virtle keeps one end as a conn, QEMU inherits the other.
func framePair() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	hostFile := os.NewFile(uintptr(fds[0]), "vmnet-host")
	guestEnd := os.NewFile(uintptr(fds[1]), "vmnet-guest")
	hostEnd, err := net.FileConn(hostFile)
	_ = hostFile.Close() // FileConn holds its own descriptor
	if err != nil {
		_ = guestEnd.Close()
		return nil, nil, fmt.Errorf("socketpair conn: %w", err)
	}
	return hostEnd, guestEnd, nil
}

// guestFile is QEMU's end of the socket, for exec.Cmd.ExtraFiles.
func (a *networkAttachment) guestFile() *os.File {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.guestEnd
}

// releaseGuestEnd closes virtle's copy of QEMU's socket end once QEMU has
// inherited it, or failed to start.
func (a *networkAttachment) releaseGuestEnd() {
	a.mu.Lock()
	f := a.guestEnd
	a.guestEnd = nil
	a.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// netdev describes the NIC for QEMU's arguments.
func (a *networkAttachment) netdev(fd int) managedNetdev {
	return managedNetdev{ID: a.id, FD: fd, MAC: a.port.MAC().String(), MTU: a.mtu}
}

func (a *networkAttachment) status() control.NetworkStatus {
	return control.NetworkStatus{ID: a.id, MAC: a.port.MAC().String(), Attached: true, Addr: a.port.Addr().String()}
}

// expose exposes a host->guest forward on the port.
func (a *networkAttachment) expose(ctx context.Context, f vm.Forward) error {
	key := keyFor(f)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return net.ErrClosed
	}
	if _, dup := a.exposures[key]; dup {
		return fmt.Errorf("forward %s %s -> %s is already exposed", key.proto, f.HostAddr, f.GuestAddr)
	}
	closer, err := a.port.Expose(ctx, f)
	if err != nil {
		return err
	}
	a.exposures[key] = closer
	return nil
}

// unexpose removes a forward exposed at Start or by expose.
func (a *networkAttachment) unexpose(f vm.Forward) error {
	key := keyFor(f)
	a.mu.Lock()
	closer, ok := a.exposures[key]
	delete(a.exposures, key)
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("forward %s %s -> %s is not exposed on this machine", key.proto, f.HostAddr, f.GuestAddr)
	}
	return closer.Close()
}

// Close releases the forwards, the port, and any socket end still held. It
// is idempotent: the launch's exit path and Suspend can both reach it.
func (a *networkAttachment) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	closers := make([]io.Closer, 0, len(a.exposures))
	for _, c := range a.exposures {
		closers = append(closers, c)
	}
	a.exposures = nil
	guestEnd := a.guestEnd
	a.guestEnd = nil
	a.mu.Unlock()
	var errs []error
	for _, c := range closers {
		errs = append(errs, c.Close())
	}
	errs = append(errs, a.port.Close())
	if guestEnd != nil {
		errs = append(errs, guestEnd.Close())
	}
	return errors.Join(errs...)
}

// networkStatuses lists the NICs configured at launch for Status.
func networkStatuses(mf *manifest.Manifest, attached *networkAttachment) []control.NetworkStatus {
	devices := mf.QEMU.Devices.Network
	if len(devices) == 0 {
		return nil
	}
	statuses := make([]control.NetworkStatus, 0, len(devices))
	for _, dev := range devices {
		if dev.Managed && attached != nil && attached.id == dev.ID {
			statuses = append(statuses, attached.status())
			continue
		}
		statuses = append(statuses, control.NetworkStatus{ID: dev.ID, MAC: dev.MacAddress})
	}
	return statuses
}
