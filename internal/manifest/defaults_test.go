package manifest

import "testing"

func TestDocumentWithDefaultsPreservesExplicitEmptyNetworks(t *testing.T) {
	document := validDocument()
	document.Networks = []NetworkInput{}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if len(manifest.QEMU.Devices.Network) != 0 {
		t.Fatalf("expected explicit empty networks to stay empty, got %#v", manifest.QEMU.Devices.Network)
	}
}

func TestDocumentWithDefaultsAddsOmittedNetwork(t *testing.T) {
	document := validDocument()
	document.Networks = nil

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := len(manifest.QEMU.Devices.Network), 1; got != want {
		t.Fatalf("network count = %d, want %d", got, want)
	}
	network := manifest.QEMU.Devices.Network[0]
	if network.ID != defaultNetworkID || network.Backend != defaultNetworkType || network.MacAddress != defaultNetworkMAC {
		t.Fatalf("unexpected default network: %#v", network)
	}
}

func TestDocumentWithDefaultsPreservesKeyResolvedDefaults(t *testing.T) {
	document := validDocument()
	document.HostName = ""
	document.WorkingDir = ""
	document.StateDir = ""
	document.Machine.Type = ""
	document.Machine.Memory = 0
	document.QEMU.QMPSocket = ""
	document.QEMU.GuestAgentSocket = ""
	document.SSH.User = ""
	document.SSH.ReadySocket = ""
	document.SSH.RetryDelay = nil
	document.VSock.CIDRange = RangeInput{}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got := manifest.Identity.HostName; got != defaultHostName {
		t.Fatalf("host name = %q, want %q", got, defaultHostName)
	}
	if got := manifest.Paths.WorkingDir; got != defaultWorkingDir {
		t.Fatalf("working dir = %q, want %q", got, defaultWorkingDir)
	}
	if got := manifest.Persistence.BaseDir; got != defaultBaseDir {
		t.Fatalf("base dir = %q, want %q", got, defaultBaseDir)
	}
	if got := manifest.Persistence.StateDir; got != defaultBaseDir {
		t.Fatalf("state dir = %q, want %q", got, defaultBaseDir)
	}
	if got, want := manifest.Paths.LockPath, ".virtie/virtie.lock"; got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	if got := manifest.QEMU.Machine.Type; got != defaultMachineType {
		t.Fatalf("machine type = %q, want %q", got, defaultMachineType)
	}
	if got := manifest.QEMU.Memory.Size; got != defaultMemorySize {
		t.Fatalf("memory = %d, want %d", got, defaultMemorySize)
	}
	if got := manifest.QEMU.QMP.SocketPath; got != defaultQMP {
		t.Fatalf("qmp socket = %q, want %q", got, defaultQMP)
	}
	if got := manifest.QEMU.GuestAgent.SocketPath; got != defaultGuestAgent {
		t.Fatalf("guest agent socket = %q, want %q", got, defaultGuestAgent)
	}
	if got := manifest.QEMU.SSHReady.SocketPath; got != defaultSSHReadySocket {
		t.Fatalf("ssh ready socket = %q, want %q", got, defaultSSHReadySocket)
	}
	if got := manifest.SSH.User; got != defaultSSHUser {
		t.Fatalf("ssh user = %q, want %q", got, defaultSSHUser)
	}
	if got := manifest.SSH.RetryDelay.Seconds(); got != defaultSSHRetryDelaySeconds {
		t.Fatalf("ssh retry delay = %v, want %v", got, defaultSSHRetryDelaySeconds)
	}
	if got := manifest.VSock.CIDRange.Start; got != defaultVSockCIDStart {
		t.Fatalf("cid start = %d, want %d", got, defaultVSockCIDStart)
	}
	if got := manifest.VSock.CIDRange.End; got != defaultVSockCIDEnd {
		t.Fatalf("cid end = %d, want %d", got, defaultVSockCIDEnd)
	}
}

func TestDocumentWithDefaultsPreservesExplicitOverridesForMovedDefaults(t *testing.T) {
	document := validDocument()
	document.HostName = "custom-host"
	document.WorkingDir = "/custom/work"
	document.StateDir = ".custom-state"
	document.Machine.Type = "q35"
	document.Machine.Memory = 2048
	document.QEMU.QMPSocket = "custom-qmp.sock"
	document.QEMU.GuestAgentSocket = "custom-qga.sock"
	document.SSH.User = "custom-user"
	document.SSH.ReadySocket = "custom-ready.sock"
	retryDelay := 2.5
	document.SSH.RetryDelay = &retryDelay
	document.VSock.CIDRange = RangeInput{Min: 10, Max: 20}

	manifest, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	if got, want := manifest.Identity.HostName, "custom-host"; got != want {
		t.Fatalf("host name = %q, want %q", got, want)
	}
	if got, want := manifest.Paths.WorkingDir, "/custom/work"; got != want {
		t.Fatalf("working dir = %q, want %q", got, want)
	}
	if got, want := manifest.Persistence.BaseDir, ".custom-state"; got != want {
		t.Fatalf("base dir = %q, want %q", got, want)
	}
	if got, want := manifest.Persistence.StateDir, ".custom-state"; got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	if got, want := manifest.Paths.LockPath, ".custom-state/custom-host.lock"; got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	if got, want := manifest.QEMU.Machine.Type, "q35"; got != want {
		t.Fatalf("machine type = %q, want %q", got, want)
	}
	if got, want := manifest.QEMU.Memory.Size.Int(), 2048; got != want {
		t.Fatalf("memory = %d, want %d", got, want)
	}
	if got, want := manifest.QEMU.QMP.SocketPath, "custom-qmp.sock"; got != want {
		t.Fatalf("qmp socket = %q, want %q", got, want)
	}
	if got, want := manifest.QEMU.GuestAgent.SocketPath, "custom-qga.sock"; got != want {
		t.Fatalf("guest agent socket = %q, want %q", got, want)
	}
	if got, want := manifest.QEMU.SSHReady.SocketPath, "custom-ready.sock"; got != want {
		t.Fatalf("ssh ready socket = %q, want %q", got, want)
	}
	if got, want := manifest.SSH.User, "custom-user"; got != want {
		t.Fatalf("ssh user = %q, want %q", got, want)
	}
	if got, want := manifest.SSH.RetryDelay.Seconds(), 2.5; got != want {
		t.Fatalf("ssh retry delay = %v, want %v", got, want)
	}
	if got, want := manifest.VSock.CIDRange.Start, 10; got != want {
		t.Fatalf("cid start = %d, want %d", got, want)
	}
	if got, want := manifest.VSock.CIDRange.End, 20; got != want {
		t.Fatalf("cid end = %d, want %d", got, want)
	}
}
