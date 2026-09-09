package firecracker

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
	"github.com/shazow/virtle/units"
	"github.com/shazow/virtle/vm"
)

func TestConfiguration(t *testing.T) {
	spec := &vm.Spec{Dir: "/work", CPUs: 2, Memory: 256 * units.Mebibyte, Kernel: vm.Kernel{Path: "kernel", Initrd: "initrd", Cmdline: `console=ttyS0 init="a b"`}, Disks: []vm.Disk{{Path: "disk", Format: "raw", ReadOnly: true, GuestPath: "/"}}}
	mf, err := (&Backend{}).resolveSpec(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var bodies []map[string]any
	c := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	if err := c.configure(t.Context(), mf.Firecracker); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/machine-config", "/boot-source", "/drives/disk0", "/actions"}) {
		t.Fatal(paths)
	}
	if bodies[0]["vcpu_count"] != float64(2) || bodies[0]["mem_size_mib"] != float64(256) {
		t.Fatal(bodies[0])
	}
	// virtle's reboot/panic policy and the root device precede the caller's
	// command line, whose quoting survives intact; no console parameter
	// without Console: print.
	if bodies[1]["kernel_image_path"] != "/work/kernel" || bodies[1]["initrd_path"] != "/work/initrd" || bodies[1]["boot_args"] != "reboot=k panic=-1 root=/dev/vda ro "+spec.Kernel.Cmdline {
		t.Fatal(bodies[1])
	}
	// virtle owns root=, so Firecracker's own root-device arguments stay off.
	if bodies[2]["path_on_host"] != "/work/disk" || bodies[2]["is_read_only"] != true || bodies[2]["is_root_device"] != false {
		t.Fatal(bodies[2])
	}
	if bodies[3]["action_type"] != "InstanceStart" {
		t.Fatal(bodies[3])
	}
}

func TestSpecValidation(t *testing.T) {
	for _, tc := range []struct {
		name, problem string
		change        func(*vm.Spec)
	}{
		{"kernel", "Kernel.Path", func(s *vm.Spec) { s.Kernel.Path = "" }},
		{"cpu", "CPUs", func(s *vm.Spec) { s.CPUs = 33 }},
		{"memory", "MiB", func(s *vm.Spec) { s.Memory = 1 }},
		{"disk", "raw", func(s *vm.Spec) { s.Disks = []vm.Disk{{Path: "disk", Format: "qcow2"}} }},
		{"file", "unsupported", func(s *vm.Spec) { s.Files = []vm.File{{GuestPath: "/file", Content: strings.NewReader("x")}} }},
		{"share", "unsupported", func(s *vm.Spec) { s.Shares = []vm.Share{{HostPath: "/work"}} }},
		{"port", "unsupported", func(s *vm.Spec) { s.Ports = []vm.Forward{{HostAddr: ":80"}} }},
		{"disk size", "unsupported", func(s *vm.Spec) { s.Disks = []vm.Disk{{Path: "disk", Size: 256 * units.Mebibyte}} }},
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
	fc := mf.Firecracker
	// Like QEMU, a zero CPU count means every host CPU (within the VMM's limit).
	if fc.CPUs != min(runtime.NumCPU(), imanifest.MaxFirecrackerCPUs) || fc.MemoryMiB != DefaultMemory.Mebibytes() || fc.Console != "off" || fc.Kernel.Cmdline != "reboot=k panic=-1" {
		t.Fatalf("defaults: %+v", fc)
	}
	if got := mf.ResolvedLockPath(); got != "/state/virtle.lock" {
		t.Fatalf("state directory override: lock at %q", got)
	}
	mf, err = (&Backend{Console: ConsolePrint}).resolveSpec(&vm.Spec{Dir: "/work", Kernel: vm.Kernel{Path: "kernel", Cmdline: "quiet"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if fc := mf.Firecracker; fc.Console != "print" || fc.Kernel.Cmdline != "console=ttyS0 reboot=k panic=-1 quiet" {
		t.Fatalf("console print: %+v", fc)
	}
}

func TestConfigurationStopsOnError(t *testing.T) {
	mf, err := (&Backend{}).resolveSpec(&vm.Spec{Kernel: vm.Kernel{Path: "kernel"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := &apiClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"fault_message":"bad config"}`))}, nil
	})}}
	if err := c.configure(t.Context(), mf.Firecracker); err == nil || calls != 1 {
		t.Fatalf("calls %d, error %v", calls, err)
	}
}
