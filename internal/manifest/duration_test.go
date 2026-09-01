package manifest

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestParseDurationAcceptsUnitsAndBareSeconds(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Duration
	}{
		{"30s", Duration(30 * time.Second)},
		{"1m30s", Duration(90 * time.Second)},
		{"500ms", Duration(500 * time.Millisecond)},
		{"5", Duration(5 * time.Second)},
		{"0.5", Duration(500 * time.Millisecond)},
		{"0", 0},
	} {
		got, err := ParseDuration(tt.in)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("parse %q: got %s want %s", tt.in, got, tt.want)
		}
	}

	if _, err := ParseDuration("nope"); err == nil || !strings.Contains(err.Error(), `invalid duration "nope"`) {
		t.Fatalf("expected parse error, got %v", err)
	}

	// Positive infinity and overflow saturate to the maximum duration.
	for _, in := range []string{"inf", "1e300"} {
		got, err := ParseDuration(in)
		if err != nil || got != Duration(math.MaxInt64) {
			t.Fatalf("parse %q: got %s err %v, want max duration", in, got, err)
		}
	}
	for _, in := range []string{"nan", "-inf"} {
		if _, err := ParseDuration(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `"1m30s"`; got != want {
		t.Fatalf("encoded: got %s want %s", got, want)
	}

	var decoded Duration
	for raw, want := range map[string]Duration{
		`"2m"`:  Duration(2 * time.Minute),
		`"2.5"`: Duration(2500 * time.Millisecond),
		`2.5`:   Duration(2500 * time.Millisecond),
		`30`:    Duration(30 * time.Second),
	} {
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if decoded != want {
			t.Fatalf("unmarshal %s: got %s want %s", raw, decoded, want)
		}
	}

	if err := json.Unmarshal([]byte(`"nope"`), &decoded); err == nil {
		t.Fatal("expected invalid duration error")
	}
	if err := json.Unmarshal([]byte(`true`), &decoded); err == nil {
		t.Fatal("expected type error")
	}
}

func TestDurationTOMLRoundTrip(t *testing.T) {
	type doc struct {
		D Duration `toml:"d"`
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc{D: Duration(500 * time.Millisecond)}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got, want := strings.TrimSpace(buf.String()), `d = "500ms"`; got != want {
		t.Fatalf("encoded: got %q want %q", got, want)
	}
	var decoded doc
	if err := toml.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.D != Duration(500*time.Millisecond) {
		t.Fatalf("round trip: got %s want 500ms", decoded.D)
	}
}

func TestDurationUnmarshalTOML(t *testing.T) {
	var d Duration
	if err := d.UnmarshalTOML(int64(30)); err != nil || d != Duration(30*time.Second) {
		t.Fatalf("int64: got %s err %v", d, err)
	}
	// A bare integer far past the int64 nanosecond range saturates instead of
	// overflowing to a negative duration.
	if err := d.UnmarshalTOML(int64(90000000000)); err != nil || d != Duration(math.MaxInt64) {
		t.Fatalf("overflowing int64: got %s err %v, want max duration", d, err)
	}
	if err := d.UnmarshalTOML(0.5); err != nil || d != Duration(500*time.Millisecond) {
		t.Fatalf("float64: got %s err %v", d, err)
	}
	if err := d.UnmarshalTOML("1m"); err != nil || d != Duration(time.Minute) {
		t.Fatalf("string: got %s err %v", d, err)
	}
	if err := d.UnmarshalTOML(true); err == nil {
		t.Fatal("expected type error for bool")
	}
}
