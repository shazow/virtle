package cloudhypervisor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	imanifest "github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/vmmhost"
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

// serialDevice is the guest's serial console on this architecture.
func serialDevice() string {
	if runtime.GOARCH == "arm64" {
		return "ttyAMA0"
	}
	return "ttyS0"
}

// recordingClient captures every request's path and decoded body.
func recordingClient(t *testing.T, status int, body string) (*apiClient, *[]string, *[]map[string]any) {
	t.Helper()
	var paths []string
	var bodies []map[string]any
	c := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		var decoded map[string]any
		if data, _ := io.ReadAll(r.Body); len(data) != 0 {
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
		}
		bodies = append(bodies, decoded)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	return c, &paths, &bodies
}

func TestConfiguration(t *testing.T) {
	spec := &vm.Spec{Dir: "/work", CPUs: 2, Memory: 256 * units.Mebibyte, Kernel: vm.Kernel{Path: "kernel", Initrd: "initrd", Cmdline: `init="a b"`}, Disks: []vm.Disk{{Path: "disk", Format: "raw", ReadOnly: true, GuestPath: "/"}, {Path: "scratch.qcow2", Format: "qcow2"}}}
	mf, err := (&Backend{}).resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	c, paths, bodies := recordingClient(t, 204, "")
	if err := c.configure(t.Context(), mf.CloudHypervisor); err != nil {
		t.Fatal(err)
	}
	// One VmConfig creates the VM, then it boots.
	if !reflect.DeepEqual(*paths, []string{"PUT /api/v1/vm.create", "PUT /api/v1/vm.boot"}) {
		t.Fatal(*paths)
	}
	create := (*bodies)[0]
	if cpus := create["cpus"].(map[string]any); cpus["boot_vcpus"] != float64(2) || cpus["max_vcpus"] != float64(2) {
		t.Fatal(cpus)
	}
	if memory := create["memory"].(map[string]any); memory["size"] != float64(256<<20) || memory["shared"] != false {
		t.Fatal(memory)
	}
	// The root device precedes the caller's command line, whose quoting
	// survives intact; no console parameter without Console: print, and no
	// reboot/panic policy on this VMM.
	if payload := create["payload"].(map[string]any); payload["kernel"] != "/work/kernel" || payload["initramfs"] != "/work/initrd" || payload["cmdline"] != "root=/dev/vda ro "+spec.Kernel.Cmdline {
		t.Fatal(payload)
	}
	disks := create["disks"].([]any)
	if disk := disks[0].(map[string]any); len(disks) != 2 || disk["path"] != "/work/disk" || disk["readonly"] != true || disk["id"] != "disk0" || disk["image_type"] != "Raw" || disk["direct"] != false {
		t.Fatal(disks)
	}
	if disk := disks[1].(map[string]any); disk["path"] != "/work/scratch.qcow2" || disk["image_type"] != "Qcow2" || disk["readonly"] != false {
		t.Fatal(disks)
	}
	// Both consoles are named explicitly: the VMM's defaults put the
	// virtio-console on the process streams and no serial anywhere.
	if create["serial"].(map[string]any)["mode"] != "Off" || create["console"].(map[string]any)["mode"] != "Off" {
		t.Fatal(create["serial"], create["console"])
	}
	if _, ok := create["fs"]; ok {
		t.Fatal("fs sent without shares")
	}
	if (*bodies)[1] != nil {
		t.Fatalf("vm.boot carried a body: %v", (*bodies)[1])
	}
}

func TestConfigurationWithShares(t *testing.T) {
	spec := &vm.Spec{Dir: "/work", Kernel: vm.Kernel{Path: "kernel", Cmdline: "quiet"}, Shares: []vm.Share{{Tag: "src", HostPath: "src", GuestPath: "/mnt"}, {Tag: "data", HostPath: "/data", ReadOnly: true}}}
	mf, err := (&Backend{Console: ConsolePrint}).resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []imanifest.CloudHypervisorShare{
		{Tag: "src", Source: "/work/src", Socket: "/work/.virtle/src.sock"},
		{Tag: "data", Source: "/data", Socket: "/work/.virtle/data.sock"},
	}
	if !reflect.DeepEqual(mf.CloudHypervisor.Shares, want) {
		t.Fatalf("shares = %+v, want %+v", mf.CloudHypervisor.Shares, want)
	}
	if runs, err := mf.ResolvedRuns(0); err != nil || len(runs) != 2 || runs[0].Exec[0] != "virtiofsd" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	c, _, bodies := recordingClient(t, 204, "")
	if err := c.configure(t.Context(), mf.CloudHypervisor); err != nil {
		t.Fatal(err)
	}
	create := (*bodies)[0]
	// virtiofsd maps the guest memory, so it has to be shared.
	if memory := create["memory"].(map[string]any); memory["shared"] != true {
		t.Fatal(memory)
	}
	fs := create["fs"].([]any)
	if len(fs) != 2 {
		t.Fatal(fs)
	}
	if share := fs[0].(map[string]any); share["tag"] != "src" || share["socket"] != "/work/.virtle/src.sock" || share["num_queues"] != float64(1) || share["queue_size"] != float64(1024) || share["id"] != "fs0" {
		t.Fatal(share)
	}
	if create["serial"].(map[string]any)["mode"] != "Tty" || create["console"].(map[string]any)["mode"] != "Off" {
		t.Fatal(create["serial"], create["console"])
	}
	if payload := create["payload"].(map[string]any); payload["cmdline"] != "console="+serialDevice()+" quiet" {
		t.Fatal(payload)
	}
}

func TestSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		name, problem string
		change        func(*vm.Spec)
	}{
		{"kernel", "Kernel.Path", func(s *vm.Spec) { s.Kernel.Path = "" }},
		{"cpu", "CPUs", func(s *vm.Spec) { s.CPUs = imanifest.MaxCloudHypervisorCPUs + 1 }},
		{"memory", "MiB", func(s *vm.Spec) { s.Memory = 1 }},
		{"disk", "raw and qcow2", func(s *vm.Spec) { s.Disks = []vm.Disk{{Path: "disk", Format: "vmdk"}} }},
		{"file", "unsupported", func(s *vm.Spec) { s.Files = []vm.File{{GuestPath: "/file", Content: strings.NewReader("x")}} }},
		{"share without tag", "Tag is required", func(s *vm.Spec) { s.Shares = []vm.Share{{HostPath: "/work"}} }},
		{"share without source", "source is required", func(s *vm.Spec) { s.Shares = []vm.Share{{Tag: "src"}} }},
		{"port", "unsupported", func(s *vm.Spec) { s.Ports = []vm.Forward{{HostAddr: ":80"}} }},
		{"disk size", "MiB-aligned", func(s *vm.Spec) { s.Disks = []vm.Disk{{Path: "disk", Size: 256*units.Mebibyte + 1}} }},
		{"disk too small to create", "at least", func(s *vm.Spec) {
			s.Kernel.Initrd = "initrd"
			s.Disks = []vm.Disk{{Path: "disk", Size: 8 * units.Mebibyte}}
		}},
		{"disk guest path", "unsupported", func(s *vm.Spec) { s.Disks = []vm.Disk{{Path: "disk", GuestPath: "/mnt"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &vm.Spec{Kernel: vm.Kernel{Path: "kernel"}}
			tc.change(s)
			_, err := (&Backend{}).resolveSpec(s, "")
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("got %v, want %s", err, tc.problem)
			}
			// Missing capabilities are detectable, validation errors are not.
			if tc.problem == "unsupported" && !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("%v does not wrap errors.ErrUnsupported", err)
			}
		})
	}
}

func TestSpecDefaults(t *testing.T) {
	mf, err := (&Backend{}).resolveSpec(&vm.Spec{Dir: "/work", Kernel: vm.Kernel{Path: "kernel"}}, "/state")
	if err != nil {
		t.Fatal(err)
	}
	ch := mf.CloudHypervisor
	// Like QEMU, a zero CPU count means every host CPU (within the VMM's limit).
	if ch.CPUs != min(runtime.NumCPU(), imanifest.MaxCloudHypervisorCPUs) || ch.MemoryMiB != DefaultMemory.Mebibytes() || ch.Console != "off" || ch.Kernel.Cmdline != "" || ch.Binary != "cloud-hypervisor" {
		t.Fatalf("defaults: %+v", ch)
	}
	if got := mf.ResolvedLockPath(); got != "/state/virtle.lock" {
		t.Fatalf("state directory override: lock at %q", got)
	}
	// An empty command line is left out of the payload altogether.
	if data, _ := json.Marshal(vmConfig(ch)); strings.Contains(string(data), "cmdline") {
		t.Fatalf("empty cmdline sent: %s", data)
	}
	mf, err = (&Backend{Console: ConsolePrint}).resolveSpec(&vm.Spec{Dir: "/work", Kernel: vm.Kernel{Path: "kernel", Cmdline: "quiet"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ch := mf.CloudHypervisor; ch.Console != "print" || ch.Kernel.Cmdline != "console="+serialDevice()+" quiet" {
		t.Fatalf("console print: %+v", ch)
	}
}

func TestConfigurationStopsOnError(t *testing.T) {
	mf, err := (&Backend{}).resolveSpec(&vm.Spec{Kernel: vm.Kernel{Path: "kernel"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	c, paths, _ := recordingClient(t, 400, `["bad config"]`)
	if err := c.configure(t.Context(), mf.CloudHypervisor); err == nil || len(*paths) != 1 || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("calls %d, error %v", len(*paths), err)
	}
}

func TestConfigurationWithTAP(t *testing.T) {
	spec := &vm.Spec{Dir: "/work", Kernel: vm.Kernel{Path: "kernel"}}
	mf, err := (&Backend{Link: TAP{Name: "tap0"}}).resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []imanifest.TapNetwork{{ID: "microvm1", Tap: "tap0", MAC: "02:02:00:00:00:01"}}; !reflect.DeepEqual(mf.CloudHypervisor.Networks, want) {
		t.Fatalf("networks = %+v, want %+v", mf.CloudHypervisor.Networks, want)
	}
	c, _, bodies := recordingClient(t, 204, "")
	if err := c.configure(t.Context(), mf.CloudHypervisor); err != nil {
		t.Fatal(err)
	}
	net := (*bodies)[0]["net"].([]any)
	if nic := net[0].(map[string]any); len(net) != 1 || nic["id"] != "microvm1" || nic["tap"] != "tap0" || nic["mac"] != "02:02:00:00:00:01" {
		t.Fatal(net)
	}
	if got := vmmhost.NetworkStatuses(mf.CloudHypervisor.Networks); len(got) != 1 || got[0].ID != "microvm1" || got[0].MAC != "02:02:00:00:00:01" || got[0].Attached || got[0].Addr != "" {
		t.Fatalf("statuses = %+v", got)
	}

	if _, err := (&Backend{Link: TAP{}}).resolveSpec(spec, ""); err == nil || !strings.Contains(err.Error(), "Name") {
		t.Fatalf("TAP without a name: %v", err)
	}
	if _, err := (&Backend{Link: TAP{Name: "tap0"}}).resolveSpec(&vm.Spec{Kernel: vm.Kernel{Path: "kernel"}, Ports: []vm.Forward{{HostAddr: ":1", GuestAddr: ":1"}}}, ""); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ports on a TAP NIC: %v", err)
	}
	// A manifest-declared network keeps its identity under a Go-set TAP.
	doc, err := imanifest.DecodeDocumentBytes([]byte("backend = \"cloud-hypervisor\"\n[kernel]\npath = \"kernel\"\n[[networks]]\nid = \"eth0\"\nmac = \"02:aa:00:00:00:01\"\ntype = \"tap\"\ntap = \"tap9\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBackendFromDocument(doc, Backend{Link: TAP{Name: "tap0"}}).(*Backend)
	mf, err = b.resolveSpec(&vm.Spec{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []imanifest.TapNetwork{{ID: "eth0", Tap: "tap0", MAC: "02:aa:00:00:00:01"}}; !reflect.DeepEqual(mf.CloudHypervisor.Networks, want) {
		t.Fatalf("networks = %+v, want %+v", mf.CloudHypervisor.Networks, want)
	}
	if b.doc.Networks[0].Tap != "tap9" {
		t.Fatal("the overlay wrote into the backend's stored document")
	}
}
