package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/units"
)

func TestDurationJSONRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(GuestExecRequest{Path: "/bin/true", Timeout: units.Duration(90 * time.Second)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"timeout":"1m30s"`) {
		t.Fatalf("unexpected encoding: %s", encoded)
	}

	var req GuestExecRequest
	if err := json.Unmarshal([]byte(`{"path":"/bin/true","timeout":"2m"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Timeout != units.Duration(2*time.Minute) {
		t.Fatalf("timeout: got %s want 2m", time.Duration(req.Timeout))
	}

	if err := json.Unmarshal([]byte(`{"timeout":"nope"}`), &req); err == nil || !strings.Contains(err.Error(), `invalid duration "nope"`) {
		t.Fatalf("expected invalid duration error, got %v", err)
	}
	if err := json.Unmarshal([]byte(`{"timeout":30}`), &req); err != nil || req.Timeout != units.Duration(30*time.Second) {
		t.Fatalf("expected bare number as seconds, got %s err %v", req.Timeout, err)
	}
}
