package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	imanifest "github.com/shazow/virtle/internal/manifest"
)

const (
	maxResponseSize = 64 * 1024
	apiPrefix       = "/api/v1/"
)

// apiClient speaks Cloud Hypervisor's HTTP API over its unix socket. Requests
// are bounded only by the caller's context: vm.boot in particular takes as
// long as the guest kernel takes to load, so a fixed per-request timeout
// would turn a slow boot into a spurious failure.
type apiClient struct {
	socket string
	http   *http.Client
}

func newAPIClient(socket string) *apiClient {
	c := &apiClient{socket: socket}
	c.http = &http.Client{
		Transport: &http.Transport{
			DialContext:            func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
			MaxResponseHeaderBytes: 8192,
			DisableKeepAlives:      true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

func (c *apiClient) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
}

// put sends one action to the named endpoint (vm.create, vm.boot, ...),
// with body, when non-nil, as JSON. It accepts 200 or 204 and discards the
// response body; other statuses report the VMM's messages, bounded and quoted.
func (c *apiClient) put(ctx context.Context, name string, body any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode cloud-hypervisor %s: %w", name, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+apiPrefix+name, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloud-hypervisor PUT %s: %w", name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("read cloud-hypervisor %s response: %w", name, err)
	}
	if len(data) > maxResponseSize {
		return fmt.Errorf("cloud-hypervisor %s response exceeds %d bytes", name, maxResponseSize)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cloud-hypervisor PUT %s: HTTP %d: %q", name, resp.StatusCode, errorMessages(data))
	}
	return nil
}

// errorMessages flattens an error body (a JSON array of messages, the
// outermost first) into one bounded line.
func errorMessages(body []byte) string {
	var messages []string
	if json.Unmarshal(body, &messages) == nil && len(messages) != 0 {
		body = []byte(strings.Join(messages, ": "))
	}
	if len(body) > 256 {
		body = body[:256]
	}
	return string(body)
}

// configure creates the VM from the resolved manifest and boots it.
func (c *apiClient) configure(ctx context.Context, cfg *imanifest.CloudHypervisor) error {
	if err := c.put(ctx, "vm.create", vmConfig(cfg)); err != nil {
		return err
	}
	return c.put(ctx, "vm.boot", nil)
}

// The VmConfig subset virtle sends, spelled as Cloud Hypervisor's API expects
// it: unknown fields are ignored silently, so every name here matters.
type (
	vmConfigBody struct {
		CPUs    cpusConfig    `json:"cpus"`
		Memory  memoryConfig  `json:"memory"`
		Payload payloadConfig `json:"payload"`
		Disks   []diskConfig  `json:"disks,omitempty"`
		Net     []netConfig   `json:"net,omitempty"`
		FS      []fsConfig    `json:"fs,omitempty"`
		Serial  consoleConfig `json:"serial"`
		Console consoleConfig `json:"console"`
	}
	cpusConfig struct {
		Boot int `json:"boot_vcpus"`
		Max  int `json:"max_vcpus"`
	}
	memoryConfig struct {
		Size   int64 `json:"size"`
		Shared bool  `json:"shared"`
	}
	payloadConfig struct {
		Kernel    string `json:"kernel"`
		Cmdline   string `json:"cmdline,omitempty"`
		Initramfs string `json:"initramfs,omitempty"`
	}
	diskConfig struct {
		Path      string `json:"path"`
		ReadOnly  bool   `json:"readonly"`
		Direct    bool   `json:"direct"`
		Serial    string `json:"serial,omitempty"`
		ID        string `json:"id"`
		ImageType string `json:"image_type"`
	}
	netConfig struct {
		Tap string `json:"tap"`
		MAC string `json:"mac"`
		ID  string `json:"id"`
	}
	fsConfig struct {
		Tag       string `json:"tag"`
		Socket    string `json:"socket"`
		NumQueues int    `json:"num_queues"`
		QueueSize int    `json:"queue_size"`
		ID        string `json:"id"`
	}
	consoleConfig struct {
		Mode string `json:"mode"`
	}
)

// imageType is Cloud Hypervisor's spelling of a resolved image format.
func imageType(format string) string {
	if format == "qcow2" {
		return "Qcow2"
	}
	return "Raw"
}

// vmConfig lowers the resolved manifest to the vm.create body. The serial
// port rides the process's standard streams (mode Tty) when the console is
// printed or interactive, and the virtio-console is always off: it defaults
// to the same streams and would garble them. Shares need the guest memory shared so the
// virtiofsd processes can map it.
func vmConfig(cfg *imanifest.CloudHypervisor) vmConfigBody {
	body := vmConfigBody{
		CPUs:    cpusConfig{Boot: cfg.CPUs, Max: cfg.CPUs},
		Memory:  memoryConfig{Size: cfg.MemoryMiB.Bytes().Int64(), Shared: len(cfg.Shares) != 0},
		Payload: payloadConfig{Kernel: cfg.Kernel.Path, Cmdline: cfg.Kernel.Cmdline, Initramfs: cfg.Kernel.InitrdPath},
		Serial:  consoleConfig{Mode: "Off"},
		Console: consoleConfig{Mode: "Off"},
	}
	if cfg.Console != imanifest.KernelSerialOff {
		body.Serial.Mode = "Tty"
	}
	for i, disk := range cfg.Disks {
		body.Disks = append(body.Disks, diskConfig{Path: disk.Path, ReadOnly: disk.ReadOnly, Direct: disk.Direct, Serial: disk.Serial, ID: "disk" + strconv.Itoa(i), ImageType: imageType(disk.Format)})
	}
	for _, network := range cfg.Networks {
		body.Net = append(body.Net, netConfig{Tap: network.Tap, MAC: network.MAC, ID: network.ID})
	}
	for i, share := range cfg.Shares {
		body.FS = append(body.FS, fsConfig{Tag: share.Tag, Socket: share.Socket, NumQueues: 1, QueueSize: 1024, ID: "fs" + strconv.Itoa(i)})
	}
	return body
}
