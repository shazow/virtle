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
	for _, input := range []string{"inf", "1e300"} {
		got, err := ParseDuration(input)
		if err != nil || got != Duration(math.MaxInt64) {
			t.Fatalf("parse %q: got %s err %v, want max duration", input, got, err)
		}
	}
	for _, input := range []string{"nan", "-inf"} {
		if _, err := ParseDuration(input); err == nil {
			t.Fatalf("expected error for %q", input)
		}
	}
}

func TestDurationEncodingRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(Duration(90 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `"1m30s"`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}

	var buf bytes.Buffer
	type document struct {
		D Duration `toml:"d"`
	}
	if err := toml.NewEncoder(&buf).Encode(document{D: Duration(500 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	var decoded document
	if err := toml.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.D != Duration(500*time.Millisecond) {
		t.Fatalf("TOML round trip = %s, want 500ms", decoded.D)
	}
}
