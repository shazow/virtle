package manifest

import (
	"fmt"
	"strings"
	"testing"
)

func TestFirecrackerReadinessAndVSockValidation(t *testing.T) {
	for _, tc := range []struct{ name, toml, json, problem string }{
		{"omitted defaults", "", "", ""},
		{"ready socket", "[ssh]\nready_socket='ready.sock'", `,"ssh":{"ready_socket":"ready.sock"}`, "ssh"},
		{"empty ready socket", "[ssh]\nready_socket=''", `,"ssh":{"ready_socket":""}`, "ssh"},
		{"default retry delay", "[ssh]\nretry_delay='500ms'", `,"ssh":{"retry_delay":"500ms"}`, "ssh"},
		{"custom retry delay", "[ssh]\nretry_delay='1s'", `,"ssh":{"retry_delay":"1s"}`, "ssh"},
		{"default ssh user", "[ssh]\nuser='agent'", `,"ssh":{"user":"agent"}`, "ssh"},
		{"disabled autoprovision", "[ssh]\nautoprovision=false", `,"ssh":{"autoprovision":false}`, "ssh"},
		{"empty exec", "[ssh]\nexec=[]", `,"ssh":{"exec":[]}`, "ssh"},
		{"empty vsock", "[vsock]", `,"vsock":{}`, "vsock"},
		{"empty cid range", "[vsock.cid_range]", `,"vsock":{"cid_range":{}}`, "vsock"},
		{"default cid range", "[vsock.cid_range]\nmin=3\nmax=65535", `,"vsock":{"cid_range":{"min":3,"max":65535}}`, "vsock"},
		{"custom cid range", "[vsock.cid_range]\nmin=100\nmax=200", `,"vsock":{"cid_range":{"min":100,"max":200}}`, "vsock"},
		{"zero cid range", "[vsock.cid_range]\nmin=0", `,"vsock":{"cid_range":{"min":0}}`, "vsock"},
	} {
		for format, input := range map[string]string{
			"toml": "backend='firecracker'\n[kernel]\npath='kernel'\n" + tc.toml,
			"json": fmt.Sprintf(`{"backend":"firecracker","kernel":{"path":"kernel"}%s}`, tc.json),
		} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				doc, err := DecodeDocumentBytes([]byte(input), "manifest."+format)
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range []Document{doc, DocumentWithDefaults(doc)} {
					_, err := candidate.Manifest()
					if tc.problem == "" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil || !strings.Contains(err.Error(), tc.problem) {
						t.Fatalf("expected unsupported %s configuration error, got %v", tc.problem, err)
					}
				}
			})
		}
	}
	for name, doc := range map[string]Document{
		"readiness":           {SSH: SSHInput{ReadySocket: "ready.sock"}},
		"default CID range":   {VSock: VSockInput{CIDRange: RangeInput{Min: 3, Max: 65535}}},
		"default retry delay": {SSH: SSHInput{RetryDelay: DefaultDocument().SSH.RetryDelay}},
	} {
		t.Run("programmatic/"+name, func(t *testing.T) {
			doc.Backend, doc.Kernel.Path = "firecracker", "kernel"
			if _, err := doc.Manifest(); err == nil {
				t.Fatal("unsupported configuration accepted")
			}
		})
	}
}

func TestBackendSelection(t *testing.T) {
	for _, tc := range []struct{ name, input, want, problem string }{
		{"default qemu", `[kernel]
path = "kernel"
initrd_path = "initrd"`, "qemu", ""},
		{"firecracker initrd optional", `backend = "firecracker"
[kernel]
path = "vmlinux"`, "firecracker", ""},
		{"unknown", `backend = "typo"`, "", "backend"},
		{"qemu requires initrd", `[kernel]
path = "kernel"`, "", "initrd_path"},
		{"firecracker requires kernel", `backend = "firecracker"`, "", "kernel.path"},
		{"explicit user network", `backend = "firecracker"
[[networks]]
type = "user"
[kernel]
path = "vmlinux"`, "", "networks"},
		{"qcow2", `backend = "firecracker"
[kernel]
path = "vmlinux"
[[mounts]]
type = "image"
source = "disk"
image.format = "qcow2"`, "", "raw"},
		{"invalid cpu", `backend = "firecracker"
[kernel]
path = "vmlinux"
[machine]
vcpu = -1`, "", "vcpu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := DecodeDocumentBytes([]byte(tc.input), "")
			if err != nil {
				t.Fatal(err)
			}
			m, err := doc.Manifest()
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("got %v, want %s", err, tc.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Backend != tc.want {
				t.Fatalf("backend = %q, want %q", m.Backend, tc.want)
			}
			if tc.want == "firecracker" && (len(m.SSH.Argv) != 0 || m.QEMU.Kernel.Path != "") {
				t.Fatal("Firecracker inherited QEMU defaults")
			}
		})
	}
}

func TestFirecrackerCompatibilityValidation(t *testing.T) {
	for _, input := range []string{
		"[qemu]\nexec=['qemu']", "[qemu]\nseccomp=true", "[machine]\ncpu='host'",
		"[machine]\ntype='q35'", "[graphics]\nbackend='gtk'", "[qemu]\nhotplug_ports=2",
		"[[mounts]]\ntype='image'\nsource='disk'\nimage.direct=true",
		"[[mounts]]\ntype='image'\nsource='disk'\nimage.size=10",
	} {
		t.Run(input, func(t *testing.T) {
			doc, err := DecodeDocumentBytes([]byte("backend='firecracker'\n[kernel]\npath='kernel'\n"+input), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := doc.Manifest(); err == nil {
				t.Fatal("unsupported or invalid configuration silently ignored")
			}
		})
	}
	doc, err := DecodeDocumentBytes([]byte("[kernel]\npath='kernel'\ninitrd_path='initrd'\n[firecracker]\nbinary='firecracker'"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.Manifest(); err == nil {
		t.Fatal("Firecracker options accepted by QEMU")
	}
	doc = Document{Backend: "firecracker", Kernel: KernelInput{Path: "kernel"}, Firecracker: FirecrackerInput{Binary: "bad\x00path"}}
	if _, err := doc.Manifest(); err == nil {
		t.Fatal("NUL path accepted")
	}
}
