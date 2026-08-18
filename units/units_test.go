package units

import "testing"

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
