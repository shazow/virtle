package launch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestAcquireCIDUsesSavedStateCID(t *testing.T) {
	cfg := cidManifest(3, 5)
	cid, err := AcquireCID(cfg, &SuspendState{CID: 4}, cidCheckerFunc(func(int) (bool, error) {
		t.Fatal("checker should not run for saved CID")
		return false, nil
	}))
	if err != nil {
		t.Fatalf("acquire saved cid: %v", err)
	}
	if cid != 4 {
		t.Fatalf("cid: got %d want 4", cid)
	}
}

func TestAcquireCIDRejectsSavedCIDOutsideRange(t *testing.T) {
	cfg := cidManifest(3, 5)
	_, err := AcquireCID(cfg, &SuspendState{CID: 7}, nil)
	if err == nil || !strings.Contains(err.Error(), "outside manifest range") {
		t.Fatalf("expected outside range error, got %v", err)
	}
}

func TestAcquireCIDUsesFirstAvailableCID(t *testing.T) {
	cfg := cidManifest(3, 5)
	var checked []int
	cid, err := AcquireCID(cfg, nil, cidCheckerFunc(func(cid int) (bool, error) {
		checked = append(checked, cid)
		return cid == 4, nil
	}))
	if err != nil {
		t.Fatalf("acquire cid: %v", err)
	}
	if cid != 4 {
		t.Fatalf("cid: got %d want 4", cid)
	}
	if got, want := checked, []int{3, 4}; !slices.Equal(got, want) {
		t.Fatalf("checked CIDs: got %v want %v", got, want)
	}
}

func TestAcquireCIDReturnsCheckerError(t *testing.T) {
	wantErr := errors.New("cid check failed")
	cfg := cidManifest(3, 5)
	_, err := AcquireCID(cfg, nil, cidCheckerFunc(func(int) (bool, error) {
		return false, wantErr
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("checker error: got %v want %v", err, wantErr)
	}
}

func TestAcquireCIDFailsWhenRangeIsExhausted(t *testing.T) {
	cfg := cidManifest(3, 5)
	_, err := AcquireCID(cfg, nil, cidCheckerFunc(func(int) (bool, error) {
		return false, nil
	}))
	if err == nil || !strings.Contains(err.Error(), "no free vsock CID") {
		t.Fatalf("expected exhausted range error, got %v", err)
	}
}

func TestAcquireCIDWithoutVSock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *SuspendState
	}{
		{name: "fresh"},
		{name: "resume zero", state: &SuspendState{CID: 0}},
		{name: "resume legacy allocation", state: &SuspendState{CID: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cidManifest(3, 5)
			cfg.QEMU.Devices.VSOCK.ID = ""
			for _, checkErr := range []error{nil, errors.New("host vsock unavailable")} {
				calls := 0
				cid, err := AcquireCID(cfg, tc.state, cidCheckerFunc(func(int) (bool, error) {
					calls++
					return false, checkErr // Every CID is occupied, or the host cannot check.
				}))
				if err != nil || cid != 0 {
					t.Errorf("disabled vsock: CID=%d err=%v, want CID 0 without error", cid, err)
				}
				if calls != 0 {
					t.Errorf("disabled vsock checked %d host CIDs", calls)
				}
			}
		})
	}
}

func cidManifest(start, end int) *manifest.Manifest {
	return &manifest.Manifest{
		VSock: manifest.VSock{CIDRange: manifest.VSockCIDRange{Start: start, End: end}},
		QEMU: manifest.QEMU{Devices: manifest.QEMUDevices{
			VSOCK: manifest.QEMUVSOCKDevice{ID: "vsock0"},
		}},
	}
}

type cidCheckerFunc func(int) (bool, error)

func (fn cidCheckerFunc) Available(cid int) (bool, error) {
	return fn(cid)
}
