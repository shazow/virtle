//go:build !linux

package firecracker

import (
	"context"
	"fmt"

	"github.com/shazow/virtle/backend"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

func (*Backend) start(context.Context, *imanifest.Manifest, bool) (backend.Machine, error) {
	return nil, fmt.Errorf("firecracker requires Linux with KVM")
}
