package launch

import (
	"fmt"

	"github.com/shazow/virtle/internal/manifest"
)

func AcquireCID(manifest *manifest.Manifest, state *SuspendState, checker VSockCIDChecker) (int, error) {
	if state == nil {
		return allocateCID(manifest, checker)
	}
	if state.CID < manifest.VSock.CIDRange.Start || state.CID > manifest.VSock.CIDRange.End {
		return 0, fmt.Errorf("saved vsock CID %d is outside manifest range %d-%d", state.CID, manifest.VSock.CIDRange.Start, manifest.VSock.CIDRange.End)
	}
	return state.CID, nil
}

func allocateCID(manifest *manifest.Manifest, checker VSockCIDChecker) (int, error) {
	for cid := manifest.VSock.CIDRange.Start; cid <= manifest.VSock.CIDRange.End; cid++ {
		if checker == nil {
			return cid, nil
		}
		available, err := checker.Available(cid)
		if err != nil {
			return 0, err
		}
		if !available {
			continue
		}
		return cid, nil
	}

	return 0, fmt.Errorf(
		"no free vsock CID in range %d-%d",
		manifest.VSock.CIDRange.Start,
		manifest.VSock.CIDRange.End,
	)
}
