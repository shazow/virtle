package vmm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vmnet"
)

// fakeNetwork records attachments and hands out ports with fixed
// identities, standing in for a vmnet.Network.
type fakeNetwork struct {
	mu       sync.Mutex
	attaches []vmnet.AttachOptions
	ports    []*fakePort
	onAttach func()
	err      error
}

func (n *fakeNetwork) MTU() int { return 1400 }

func (n *fakeNetwork) Attach(_ context.Context, link vmnet.Link, opts vmnet.AttachOptions) (vmnet.Port, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.onAttach != nil {
		n.onAttach()
	}
	n.attaches = append(n.attaches, opts)
	if n.err != nil {
		return nil, n.err
	}
	last := byte(2 + len(n.ports))
	mac := opts.MAC
	if mac == nil {
		mac = net.HardwareAddr{0x0a, 0x56, 192, 168, 127, last}
	}
	addr := opts.Addr
	if !addr.IsValid() {
		addr = netip.AddrFrom4([4]byte{192, 168, 127, last})
	}
	p := &fakePort{link: link, mac: mac, addr: addr}
	n.ports = append(n.ports, p)
	return p, nil
}

type fakePort struct {
	link vmnet.Link
	mac  net.HardwareAddr
	addr netip.Addr

	mu      sync.Mutex
	exposed []vm.Forward
	removed []vm.Forward
	closed  bool
}

func (p *fakePort) Addr() netip.Addr      { return p.addr }
func (p *fakePort) MAC() net.HardwareAddr { return p.mac }

func (p *fakePort) Expose(_ context.Context, f vm.Forward) (io.Closer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, net.ErrClosed
	}
	p.exposed = append(p.exposed, f)
	return closerFunc(func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.removed = append(p.removed, f)
		return nil
	}), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return p.link.Close()
}

func (p *fakePort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// managedManifest is validManifest with its NIC on a virtle network.
func managedManifest(dir string) *manifest.Manifest {
	cfg := validManifest(dir)
	cfg.QEMU.Devices.Network = []manifest.QEMUNetDevice{{
		ID:        "net0",
		Backend:   "stream",
		Transport: "pci",
		Managed:   true,
		Forward:   []manifest.HotplugForward{{Proto: "tcp", Host: "127.0.0.1:8022", Guest: ":22"}},
	}}
	return cfg
}

func newNetworkTestManager(network vmnet.Network, runner *launchRunner) *manager {
	return &manager{
		network:           network,
		locker:            &fileLocker{},
		runner:            runner,
		qmpDialer:         &fakeQMPDialer{client: &fakeQMPClient{}},
		socketWaiter:      &fakeSocketWaiter{},
		logger:            slog.New(slog.DiscardHandler),
		qmpConnectTimeout: time.Second,
		qmpRetryDelay:     time.Millisecond,
		shutdownDelay:     time.Millisecond,
	}
}

func TestStartWithPlanAttachesVirtleNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := managedManifest(tmpDir)
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}

	var qemuStarted, attachedFirst atomic.Bool
	var extraFiles int
	network := &fakeNetwork{onAttach: func() { attachedFirst.Store(!qemuStarted.Load()) }}
	runner := &launchRunner{onStart: func(name string, cmd *exec.Cmd) {
		if strings.HasPrefix(name, "qemu-system") {
			qemuStarted.Store(true)
			extraFiles = len(cmd.ExtraFiles)
		}
	}}
	m := newNetworkTestManager(network, runner)
	egress := &vm.Egress{Allow: []vm.Reach{{Host: "example.com"}}}
	plan, err := m.planLaunch(launch.Spec{Manifest: cfg, Options: launch.Options{Resume: ResumeModeNo, Egress: egress}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}
	running, err := m.startWithPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	defer running.Close()

	if len(network.attaches) != 1 || len(network.ports) != 1 {
		t.Fatalf("attaches = %d, ports = %d, want one each", len(network.attaches), len(network.ports))
	}
	opts, port := network.attaches[0], network.ports[0]
	if opts.Name != "agent-sandbox" || opts.Egress != egress || opts.MAC != nil || opts.Addr.IsValid() {
		t.Errorf("attach options = %+v, want the machine name and its egress with no fixed identity", opts)
	}
	if !attachedFirst.Load() {
		t.Error("QEMU started before the network was attached")
	}
	args := strings.Join(runner.qemuArgs(), " ")
	for _, want := range []string{
		"-netdev stream,id=net0,addr.type=fd,addr.str=3",
		"-device virtio-net-pci,netdev=net0,mac=" + port.mac.String() + ",host_mtu=1400",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "hostfwd") {
		t.Errorf("qemu args carry a slirp forward: %s", args)
	}
	if extraFiles != 1 {
		t.Errorf("QEMU inherited %d files, want the network socket", extraFiles)
	}
	if want := []vm.Forward{{Proto: vm.TCP, HostAddr: "127.0.0.1:8022", GuestAddr: ":22"}}; len(port.exposed) != 1 || port.exposed[0] != want[0] {
		t.Errorf("exposed forwards = %+v, want %+v", port.exposed, want)
	}
	status, err := running.runtime.Status(context.Background(), control.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := control.NetworkStatus{ID: "net0", MAC: port.mac.String(), Attached: true, Addr: "192.168.127.2"}
	if len(status.Networks) != 1 || status.Networks[0] != want {
		t.Errorf("Status.Networks = %+v, want [%+v]", status.Networks, want)
	}

	// Forwards attached at runtime go to the port, whether through the
	// control socket's hotplug feature or the backend's Attach.
	feature := m.hotplugFeature(running.runtime.QMP())
	extra := vm.Forward{HostAddr: "127.0.0.1:8080", GuestAddr: ":80"}
	resp, err := feature.Hotplug(context.Background(), control.HotplugRequest{Device: &control.DeviceRequest{Forward: &extra}})
	if err != nil {
		t.Fatalf("hotplug forward: %v", err)
	}
	if !strings.HasPrefix(resp.ID, "fwd-") || len(port.exposed) != 2 {
		t.Errorf("hotplug forward: id %q, exposed %+v", resp.ID, port.exposed)
	}
	if _, err := feature.Hotplug(context.Background(), control.HotplugRequest{Device: &control.DeviceRequest{Forward: &extra}, Detach: true}); err != nil {
		t.Fatalf("detach forward: %v", err)
	}
	if len(port.removed) != 1 || port.removed[0] != extra {
		t.Errorf("removed forwards = %+v, want the hotplugged one", port.removed)
	}

	if err := running.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !port.isClosed() {
		t.Error("the port outlived the runtime")
	}
	if _, err := port.link.ReadFrame(make([]byte, 64)); err == nil {
		t.Error("the network's end of the socket is still open")
	}
}

func TestStartWithPlanRejectsVirtleNetworkWithoutNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := managedManifest(tmpDir)
	runner := &launchRunner{}
	m := newNetworkTestManager(nil, runner)
	plan, err := m.planLaunch(launch.Spec{Manifest: cfg, Options: launch.Options{Resume: ResumeModeNo}})
	if err != nil {
		t.Fatalf("plan launch: %v", err)
	}
	running, err := m.startWithPlan(context.Background(), plan)
	if err == nil {
		running.Close()
		t.Fatal("a virtle NIC started without a network")
	}
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "qemu.Backend.Network") {
		t.Fatalf("error = %v, want ErrUnsupported naming qemu.Backend.Network", err)
	}
	for _, name := range runner.startedNames() {
		if strings.HasPrefix(name, "qemu-system") {
			t.Fatal("QEMU was started anyway")
		}
	}
}

func TestAttachNetworkKeepsSavedIdentityAndRecordsIt(t *testing.T) {
	cfg := managedManifest(t.TempDir())
	network := &fakeNetwork{}
	m := &manager{network: network, launchManifest: cfg, logger: slog.New(slog.DiscardHandler)}
	plan := &launch.Plan{
		Manifest:    cfg,
		ResumeState: &launch.SuspendState{NetworkMAC: "02:aa:bb:cc:dd:ee", NetworkAddr: "192.168.127.9"},
	}
	attached, err := m.attachNetwork(context.Background(), plan)
	if err != nil {
		t.Fatalf("attachNetwork: %v", err)
	}
	opts := network.attaches[0]
	if opts.MAC.String() != "02:aa:bb:cc:dd:ee" || opts.Addr != netip.MustParseAddr("192.168.127.9") {
		t.Fatalf("resume attached with %s/%s, want the saved identity", opts.MAC, opts.Addr)
	}
	m.attachedNet = attached
	state := m.suspendState("qmp.sock", "/tmp/agent.vmstate", 5)
	if state.NetworkMAC != "02:aa:bb:cc:dd:ee" || state.NetworkAddr != "192.168.127.9" || state.CID != 5 {
		t.Fatalf("suspend state = %+v, want the port identity", state)
	}
	if err := attached.Close(); err != nil {
		t.Fatal(err)
	}
	if err := attached.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := network.ports[0].link.ReadFrame(make([]byte, 64)); err == nil {
		t.Fatal("Close left the socket open")
	}

	// Without saved state, nothing is fixed and the network allocates.
	network.attaches = nil
	plan.ResumeState = nil
	attached, err = m.attachNetwork(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	m.attachedNet = attached
	if opts := network.attaches[0]; opts.MAC != nil || opts.Addr.IsValid() {
		t.Fatalf("fresh start fixed %s/%s", opts.MAC, opts.Addr)
	}
	if m.suspendState("qmp.sock", "x", 1).NetworkAddr != "192.168.127.3" {
		t.Fatal("suspend state does not follow the attached port")
	}
}

type checkpointNetwork struct {
	fakeNetwork
	saved      vmnet.NetworkState
	restored   []vmnet.NetworkState
	onRestore  func() error
	onSave     func()
	restoreErr error
}

func (n *checkpointNetwork) SaveNetworkState() vmnet.NetworkState {
	if n.onSave != nil {
		n.onSave()
	}
	return n.saved
}

func (n *checkpointNetwork) RestoreNetworkState(state vmnet.NetworkState) error {
	if n.onRestore != nil {
		if err := n.onRestore(); err != nil {
			return err
		}
	}
	n.restored = append(n.restored, state)
	return n.restoreErr
}

func TestNetworkCheckpointResume(t *testing.T) {
	cfg := managedManifest(t.TempDir())
	cfg.Persistence.StateDir = ".virtle"
	cfg.Paths.RuntimeDir = manifest.RuntimeDir{Mode: manifest.RuntimeDirPath, Path: ".virtle"}
	checkpoint := vmnet.NetworkState{
		FakeIPRange: netip.MustParsePrefix("198.18.0.0/15"),
		Bindings:    []vmnet.DNSBinding{{Name: "api.example", Addr: netip.MustParseAddr("198.18.0.1")}},
		Tokens:      map[string]string{"api": "guest-token"},
	}
	qmp := &fakeQMPClient{status: "running"}
	network := &checkpointNetwork{saved: checkpoint, onSave: func() {
		qmp.mu.Lock()
		defer qmp.mu.Unlock()
		if qmp.status != "paused" || qmp.migrateCalls != 1 {
			t.Error("network state was captured before the VM migration completed")
		}
	}}
	m := newNetworkTestManager(network, &launchRunner{})
	m.launchManifest = cfg
	attached, err := m.attachNetwork(t.Context(), &launch.Plan{Manifest: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	m.attachedNet = attached
	if err := m.saveSuspendStateConnected(t.Context(), "qmp.sock", qmp, 7, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	state, err := launch.ReadSuspendState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != StateVersion || !reflect.DeepEqual(state.NetworkState, &checkpoint) {
		t.Fatalf("saved network checkpoint = %+v, want %+v at version %q", state.NetworkState, checkpoint, StateVersion)
	}
	if err := attached.Close(); err != nil {
		t.Fatal(err)
	}

	// A freshly constructed backend must restore the checkpoint while it
	// holds the runtime lock, before attaching the resumed guest's NIC.
	replacement := &checkpointNetwork{onRestore: func() error {
		lock, err := (&fileLocker{}).Acquire(cfg.ResolvedLockPath())
		if err == nil {
			_ = lock.Release()
			return errors.New("network restore ran without the runtime lock")
		}
		return nil
	}}
	replacement.onAttach = func() {
		if len(replacement.restored) != 1 || !reflect.DeepEqual(replacement.restored[0], checkpoint) {
			t.Error("guest attached before its network checkpoint was restored")
		}
	}
	resumed := newNetworkTestManager(replacement, &launchRunner{})
	plan, err := resumed.planLaunch(launch.Spec{Manifest: cfg, Options: launch.Options{Resume: ResumeModeForce}})
	if err != nil {
		t.Fatalf("plan resume: %v", err)
	}
	running, err := resumed.startWithPlan(t.Context(), plan)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer running.Close()
	if len(replacement.attaches) != 1 {
		t.Fatalf("network attached %d times, want once", len(replacement.attaches))
	}
	if got := replacement.attaches[0]; got.MAC.String() != state.NetworkMAC || got.Addr.String() != state.NetworkAddr {
		t.Fatalf("resumed NIC identity = %s/%s, want %s/%s", got.MAC, got.Addr, state.NetworkMAC, state.NetworkAddr)
	}
}

func TestNetworkRestoreFailureStopsLaunch(t *testing.T) {
	restoreErr := errors.New("conflicting DNS binding")
	for _, test := range []struct {
		name    string
		capable bool
		nic     bool
		wantErr error
	}{
		{name: "unsupported network", nic: true, wantErr: errors.ErrUnsupported},
		{name: "invalid checkpoint", capable: true, nic: true, wantErr: restoreErr},
		{name: "missing NIC", capable: true, wantErr: errors.ErrUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			plain := &fakeNetwork{}
			var network vmnet.Network = plain
			if test.capable {
				n := &checkpointNetwork{restoreErr: restoreErr}
				plain, network = &n.fakeNetwork, n
			}
			cfg := managedManifest(t.TempDir())
			if !test.nic {
				cfg.QEMU.Devices.Network = nil
			}
			runner := &launchRunner{}
			m := newNetworkTestManager(network, runner)
			plan, err := m.planLaunch(launch.Spec{Manifest: cfg})
			if err != nil {
				t.Fatal(err)
			}
			statePath, err := launch.PrepareVMStateFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			plan.ResumeState = &launch.SuspendState{CID: 7, VMStatePath: statePath, NetworkState: &vmnet.NetworkState{}}
			running, err := m.startWithPlan(t.Context(), plan)
			if running != nil {
				_ = running.Close()
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("resume error = %v, want %v", err, test.wantErr)
			}
			if len(plain.attaches) != 0 || len(runner.startedNames()) != 0 {
				t.Fatal("restore failure allowed a port or process to start")
			}
		})
	}
}

func TestNetworkRestoreNeedsRuntimeLock(t *testing.T) {
	cfg := managedManifest(t.TempDir())
	lock, err := (&fileLocker{}).Acquire(cfg.ResolvedLockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	network := &checkpointNetwork{}
	m := newNetworkTestManager(network, &launchRunner{})
	plan, err := m.planLaunch(launch.Spec{Manifest: cfg})
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := launch.PrepareVMStateFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan.ResumeState = &launch.SuspendState{CID: 7, VMStatePath: statePath, NetworkState: &vmnet.NetworkState{}}
	running, err := m.startWithPlan(t.Context(), plan)
	if running != nil {
		_ = running.Close()
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("concurrent launch error = %v, want the runtime lock to be busy", err)
	}
	if len(network.restored) != 0 || len(network.attaches) != 0 {
		t.Fatal("failed launch changed network state before owning the runtime lock")
	}
}

func TestNetworkAttachmentExposeAndUnexpose(t *testing.T) {
	hostEnd, guestEnd, err := framePair()
	if err != nil {
		t.Fatal(err)
	}
	port := &fakePort{link: vmnet.QEMUStream(hostEnd, 1500), mac: net.HardwareAddr{2, 0, 0, 0, 0, 1}, addr: netip.MustParseAddr("192.168.127.2")}
	a := &networkAttachment{id: "net0", port: port, mtu: 1500, guestEnd: guestEnd, exposures: map[forwardKey]io.Closer{}}
	f := vm.Forward{HostAddr: "127.0.0.1:8080", GuestAddr: ":80"}
	if err := a.expose(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := a.expose(context.Background(), f); err == nil {
		t.Fatal("a duplicate forward was exposed")
	}
	if err := a.unexpose(f); err != nil || len(port.removed) != 1 {
		t.Fatalf("unexpose = %v, removed %+v", err, port.removed)
	}
	if err := a.unexpose(f); err == nil {
		t.Fatal("unexpose of an unknown forward succeeded")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.expose(context.Background(), f); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expose after Close = %v, want net.ErrClosed", err)
	}
}

func TestBuildQEMUCommandNetdevs(t *testing.T) {
	cfg := managedManifest(t.TempDir())
	t.Run("managed", func(t *testing.T) {
		hostEnd, guestEnd, err := framePair()
		if err != nil {
			t.Fatal(err)
		}
		port := &fakePort{link: vmnet.QEMUStream(hostEnd, 9000), mac: net.HardwareAddr{2, 0, 0, 0, 0, 7}, addr: netip.MustParseAddr("10.0.0.7")}
		a := &networkAttachment{id: "net0", port: port, mtu: 9000, guestEnd: guestEnd, exposures: map[forwardKey]io.Closer{}}
		defer a.Close()
		cmd, err := buildQEMUCommand(cfg, 42, false, io.Discard, nil, a)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		args := strings.Join(commandArgs(cmd), " ")
		for _, want := range []string{
			"-netdev stream,id=net0,addr.type=fd,addr.str=3",
			"-device virtio-net-pci,netdev=net0,mac=02:00:00:00:00:07,host_mtu=9000",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("args lack %q:\n%s", want, args)
			}
		}
		if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != guestEnd {
			t.Errorf("ExtraFiles = %v, want the guest end of the socket", cmd.ExtraFiles)
		}
	})
	t.Run("managed without attachment", func(t *testing.T) {
		if _, err := buildQEMUCommand(cfg, 42, false, io.Discard, nil, nil); err == nil {
			t.Fatal("a managed NIC was built without its port")
		}
	})
	t.Run("tap", func(t *testing.T) {
		tapCfg := validManifest(t.TempDir())
		tapCfg.QEMU.Devices.Network = []manifest.QEMUNetDevice{{
			ID: "net0", Backend: "tap", MacAddress: "02:02:00:00:00:01", Transport: "mmio",
			NetdevOptions: []string{"ifname=tap0", "script=no", "downscript=no"},
		}}
		cmd, err := buildQEMUCommand(tapCfg, 42, false, io.Discard, nil, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		args := strings.Join(commandArgs(cmd), " ")
		if want := "-netdev tap,id=net0,ifname=tap0,script=no,downscript=no -device virtio-net-device,netdev=net0,mac=02:02:00:00:00:01"; !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
		if len(cmd.ExtraFiles) != 0 {
			t.Errorf("a tap NIC inherited files: %v", cmd.ExtraFiles)
		}
	})
}
