package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	imanifest "github.com/shazow/virtle/internal/manifest"
)

const maxResponseSize = 64 * 1024

// apiClient speaks Firecracker's HTTP API over its unix socket. Requests are
// bounded only by the caller's context: InstanceStart in particular takes as
// long as the guest kernel takes to load, so a fixed per-request timeout would
// turn a slow boot into a spurious failure.
type apiClient struct {
	socket string
	http   *http.Client
}

type action struct {
	Type string `json:"action_type"`
}

func (c *apiClient) configure(ctx context.Context, cfg *imanifest.Firecracker) error {
	machine := struct {
		CPUs   int   `json:"vcpu_count"`
		Memory int64 `json:"mem_size_mib"`
	}{cfg.CPUs, int64(cfg.MemoryMiB)}
	if err := c.put(ctx, "/machine-config", machine); err != nil {
		return err
	}
	boot := struct {
		Kernel string `json:"kernel_image_path"`
		Initrd string `json:"initrd_path,omitempty"`
		Args   string `json:"boot_args"`
	}{cfg.Kernel.Path, cfg.Kernel.InitrdPath, cfg.Kernel.Cmdline}
	if err := c.put(ctx, "/boot-source", boot); err != nil {
		return err
	}
	for i, disk := range cfg.Disks {
		drive := struct {
			ID       string `json:"drive_id"`
			Path     string `json:"path_on_host"`
			Root     bool   `json:"is_root_device"`
			ReadOnly bool   `json:"is_read_only"`
		}{fmt.Sprintf("disk%d", i), disk.Path, false, disk.ReadOnly}
		if err := c.put(ctx, "/drives/"+drive.ID, drive); err != nil {
			return err
		}
	}
	return c.put(ctx, "/actions", action{Type: "InstanceStart"})
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

// dial connects to the API socket; Start polls it to learn when Firecracker
// is accepting configuration.
func (c *apiClient) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
}

func (c *apiClient) put(ctx context.Context, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode firecracker %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("read firecracker %s response: %w", path, err)
	}
	if len(body) > maxResponseSize {
		return fmt.Errorf("firecracker %s response exceeds %d bytes", path, maxResponseSize)
	}
	if resp.StatusCode != http.StatusNoContent {
		var fault struct {
			Message string `json:"fault_message"`
		}
		if json.Unmarshal(body, &fault) == nil && fault.Message != "" {
			body = []byte(fault.Message)
		}
		// Bound and quote diagnostics: even a local VMM response is untrusted.
		if len(body) > 256 {
			body = body[:256]
		}
		return fmt.Errorf("firecracker PUT %s: HTTP %d: %q", path, resp.StatusCode, body)
	}
	return nil
}
