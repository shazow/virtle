package units

import (
	"encoding/json"
	"testing"
)

func TestBytesConstants(t *testing.T) {
	if got, want := (2048 * Mebibyte).Int64(), int64(2147483648); got != want {
		t.Errorf("2048*Mebibyte = %d, want %d", got, want)
	}
	if got, want := (2 * Gibibyte).Mebibytes(), int64(2048); got != want {
		t.Errorf("(2*Gibibyte).Mebibytes() = %d, want %d", got, want)
	}
}

func TestBytesString(t *testing.T) {
	cases := []struct {
		in   Bytes
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{Kibibyte, "1KiB"},
		{1536 * Byte, "1536B"},
		{2048 * Mebibyte, "2GiB"},
		{512 * Mebibyte, "512MiB"},
		{3 * Tebibyte, "3TiB"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Bytes(%d).String() = %q, want %q", int64(c.in), got, c.want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want Bytes
	}{
		{"0B", 0},
		{"512B", 512},
		{"1536", 1536},
		{"1KiB", Kibibyte},
		{"512MiB", 512 * Mebibyte},
		{"2GiB", 2 * Gibibyte},
		{"3TiB", 3 * Tebibyte},
	}
	for _, c := range cases {
		got, err := ParseBytes(c.in)
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, in := range []string{"", "MiB", "-1MiB", "1.5GiB", "1MB", "1 MiB", "9999999999TiB"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) succeeded, want error", in)
		}
	}
}

func TestBytesRoundTripsText(t *testing.T) {
	for _, b := range []Bytes{0, 1536, 512 * Mebibyte, 2 * Gibibyte} {
		text, err := b.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%d): %v", b, err)
		}
		var back Bytes
		if err := back.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q): %v", text, err)
		}
		if back != b {
			t.Errorf("round trip %d -> %q -> %d", b, text, back)
		}
	}

	var decoded struct{ Memory Bytes }
	if err := json.Unmarshal([]byte(`{"Memory": "2GiB"}`), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.Memory != 2*Gibibyte {
		t.Errorf("Memory = %s, want 2GiB", decoded.Memory)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if got, want := string(encoded), `{"Memory":"2GiB"}`; got != want {
		t.Errorf("json.Marshal = %s, want %s", got, want)
	}
}
