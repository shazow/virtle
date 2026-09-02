package units

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestBytesConstants(t *testing.T) {
	if got, want := (2048 * Mebibyte).Int64(), int64(2147483648); got != want {
		t.Errorf("2048*Mebibyte = %d, want %d", got, want)
	}
	if got, want := (2 * Gibibyte).Mebibytes(), MiB(2048); got != want {
		t.Errorf("(2*Gibibyte).Mebibytes() = %d, want %d", got, want)
	}
	if got := MiB(2048).Bytes(); got != 2*Gibibyte {
		t.Errorf("MiB(2048).Bytes() = %v, want 2GiB", got)
	}
}

func TestBytesEncodingRoundTrip(t *testing.T) {
	const want = 2 * Gibibyte
	parsed, err := ParseBytes("2GiB")
	if err != nil || parsed != want {
		t.Fatalf("ParseBytes(2GiB) = %v, %v", parsed, err)
	}

	text, err := want.MarshalText()
	if err != nil || string(text) != "2GiB" {
		t.Fatalf("MarshalText = %q, %v", text, err)
	}
	var fromText Bytes
	if err := fromText.UnmarshalText(text); err != nil || fromText != want {
		t.Fatalf("UnmarshalText = %v, %v", fromText, err)
	}

	data, err := json.Marshal(want)
	if err != nil || string(data) != `"2GiB"` {
		t.Fatalf("MarshalJSON = %s, %v", data, err)
	}
	var fromJSON Bytes
	if err := json.Unmarshal(data, &fromJSON); err != nil || fromJSON != want {
		t.Fatalf("UnmarshalJSON = %v, %v", fromJSON, err)
	}

	type document struct {
		Size Bytes `toml:"size"`
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(document{Size: want}); err != nil {
		t.Fatalf("encode TOML: %v", err)
	}
	if got := strings.TrimSpace(encoded.String()); got != `size = "2GiB"` {
		t.Fatalf("TOML = %q", got)
	}
	var fromTOML document
	if err := toml.Unmarshal(encoded.Bytes(), &fromTOML); err != nil || fromTOML.Size != want {
		t.Fatalf("decode TOML = %v, %v", fromTOML.Size, err)
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
