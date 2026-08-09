package qmpclient

import (
	"context"
	"fmt"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

// SetBalloonSize sets the guest's logical memory size through the virtio
// balloon device. The VM must have been started with a balloon device.
func SetBalloonSize(ctx context.Context, client RawRunner, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return fmt.Errorf("balloon size must be positive, got %d bytes", sizeBytes)
	}
	return client.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Balloon(sizeBytes); err != nil {
			return fmt.Errorf("qmp balloon: %w", err)
		}
		return nil
	})
}
