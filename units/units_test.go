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
	tests := map[string]Bytes{
		"0B": 0, "1536B": 1536, "2KiB": 2 * Kibibyte,
		"512MiB": 512 * Mebibyte, "2GiB": 2 * Gibibyte, "1TiB": Tebibyte,
	}
	for input, want := range tests {
		got, err := ParseBytes(input)
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", input, err)
		} else if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", input, got, want)
		}
	}
	for _, input := range []string{"", "1", "1MB", "-1B", "9223372036854775807TiB"} {
		if _, err := ParseBytes(input); err == nil {
			t.Errorf("ParseBytes(%q) unexpectedly succeeded", input)
		}
	}
}

func TestBytesTextRoundTrip(t *testing.T) {
	want := 2 * Gibibyte
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `"2GiB"` {
		t.Fatalf("JSON = %s, want %q", data, `"2GiB"`)
	}
	var got Bytes
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %d, want %d", got, want)
	}
}
