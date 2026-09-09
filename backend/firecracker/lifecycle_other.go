//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

func (*Backend) start(_ context.Context, _ *imanifest.Manifest, ephemeralState string) (backend.Machine, error) {
	if ephemeralState != "" {
		_ = os.RemoveAll(ephemeralState)
	}
	return nil, fmt.Errorf("firecracker requires Linux with KVM: %w", errors.ErrUnsupported)
}
