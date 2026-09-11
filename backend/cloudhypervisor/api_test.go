package cloudhypervisor

import (
	"context"
	"errors"
	imanifest "github.com/shazow/virtle/internal/manifest"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPICall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"action", 204, "", ""},
		{"read", 200, `{"version":"v52.0"}`, ""},
		{"messages", 400, `["Error creating VM: kernel missing","No such file"]`, "kernel missing: No such file"},
		{"malformed", 500, "\x1b[31muntrusted", `\x1b`},
		{"bounded", 400, strings.Repeat("x", 65537), "exceeds"},
		{"not found", 404, "[]", "HTTP 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "PUT" || r.URL.Path != "/api/v1/vm.boot" || r.Header.Get("Content-Type") != "" {
					t.Fatalf("request: %v", r)
				}
				if body, _ := io.ReadAll(r.Body); len(body) != 0 {
					t.Fatalf("body: %s", body)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}}
			var out map[string]any
			err := client.call(t.Context(), http.MethodPut, "vm.boot", nil, &out)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.status == 200 && out["version"] != "v52.0" {
					t.Fatalf("decoded %v", out)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if len(err.Error()) > 2048 || strings.ContainsRune(err.Error(), '\x1b') {
				t.Fatal("unbounded or unsafe error")
			}
		})
	}
}

func TestAPICallEncodesBody(t *testing.T) {
	client := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "PUT" || r.URL.Path != "/api/v1/vm.create" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request: %v", r)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"mode":"Tty"}` {
			t.Fatalf("body: %s", body)
		}
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})}}
	if err := client.call(t.Context(), http.MethodPut, "vm.create", consoleConfig{Mode: "Tty"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAPIContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}}
	if err := client.call(ctx, http.MethodPut, "vm.boot", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// TestVMConfigDisks covers the disk lowering: the format as Cloud Hypervisor
// spells it, and the serial and direct I/O options.
func TestVMConfigDisks(t *testing.T) {
	body := vmConfig(&imanifest.CloudHypervisor{VMM: imanifest.VMM{Disks: []imanifest.VMMDisk{
		{Path: "/a.img", Format: "raw"},
		{Path: "/b.qcow2", Format: "qcow2", ReadOnly: true, Serial: "data", Direct: true},
	}}})
	want := []diskConfig{
		{Path: "/a.img", ID: "disk0", ImageType: "Raw"},
		{Path: "/b.qcow2", ReadOnly: true, Direct: true, Serial: "data", ID: "disk1", ImageType: "Qcow2"},
	}
	if !reflect.DeepEqual(body.Disks, want) {
		t.Fatalf("disks = %+v, want %+v", body.Disks, want)
	}
}
