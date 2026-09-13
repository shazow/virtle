package vmmhost

import (
	"reflect"
	"slices"
	"testing"

	"github.com/shazow/virtle/backend"
)

func TestStatusSnapshot(t *testing.T) {
	want := backend.Status{
		State: backend.StateReady,
		PID:   123,
		Networks: []backend.NetworkStatus{{
			ID:  "eth0",
			MAC: "02:00:00:00:00:01",
		}},
	}
	m := &Machine{status: want}
	m.status.Networks = slices.Clone(want.Networks)
	status, err := m.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("Status = %+v, want %+v", status, want)
	}
	// Callers can edit a snapshot while another caller reads machine status.
	status.Networks[0].MAC = "02:00:00:00:00:02"
	again, err := m.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("Status after editing snapshot = %+v, want %+v", again, want)
	}
}
