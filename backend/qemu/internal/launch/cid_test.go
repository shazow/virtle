package launch

import (
	"errors"
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
	cid, err := AcquireCID(cfg, nil, cidCheckerFunc(func(cid int) (bool, error) {
		return cid == 4, nil
	}))
	if err != nil {
		t.Fatalf("acquire cid: %v", err)
	}
	if cid != 4 {
		t.Fatalf("cid: got %d want 4", cid)
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

func cidManifest(start, end int) *manifest.Manifest {
	return &manifest.Manifest{VSock: manifest.VSock{CIDRange: manifest.VSockCIDRange{Start: start, End: end}}}
}

type cidCheckerFunc func(int) (bool, error)

func (fn cidCheckerFunc) Available(cid int) (bool, error) {
	return fn(cid)
}
