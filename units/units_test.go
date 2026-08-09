package units_test

import (
	"testing"

	"github.com/shazow/virtle/units"
)

func TestBytesMultipliers(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  units.Bytes
		want int64
	}{
		{name: "byte", got: units.Byte, want: 1},
		{name: "kibibyte", got: units.Kibibyte, want: 1024},
		{name: "mebibyte", got: units.Mebibyte, want: 1024 * 1024},
		{name: "gibibyte", got: units.Gibibyte, want: 1024 * 1024 * 1024},
		{name: "scaled", got: 2048 * units.Mebibyte, want: 2048 * 1024 * 1024},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got.Int64(); got != tt.want {
				t.Fatalf("Int64() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBytesString(t *testing.T) {
	for _, tt := range []struct {
		size units.Bytes
		want string
	}{
		{size: 0, want: "0 B"},
		{size: 512 * units.Byte, want: "512 B"},
		{size: 4 * units.Kibibyte, want: "4 KiB"},
		{size: 2048 * units.Mebibyte, want: "2 GiB"},
		{size: 1536 * units.Mebibyte, want: "1536 MiB"},
		{size: 1025 * units.Byte, want: "1025 B"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.size.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBytesToMiB(t *testing.T) {
	t.Run("whole mebibytes", func(t *testing.T) {
		got, err := units.BytesToMiB(2048 * units.Mebibyte)
		if err != nil {
			t.Fatalf("BytesToMiB() error = %v", err)
		}
		if got != units.MiB(2048) {
			t.Fatalf("BytesToMiB() = %d, want 2048", got)
		}
	})

	t.Run("partial mebibyte", func(t *testing.T) {
		if _, err := units.BytesToMiB(units.Mebibyte + units.Kibibyte); err == nil {
			t.Fatal("BytesToMiB() error = nil, want a whole-mebibyte error")
		}
	})

	t.Run("negative", func(t *testing.T) {
		if _, err := units.BytesToMiB(-units.Mebibyte); err == nil {
			t.Fatal("BytesToMiB() error = nil, want a negative-size error")
		}
	})
}

func TestMiBRoundTrip(t *testing.T) {
	size := units.MiB(1024)
	if got := size.Bytes(); got != units.Gibibyte {
		t.Fatalf("Bytes() = %s, want %s", got, units.Gibibyte)
	}
	if got := size.Bytes().MiB(); got != size {
		t.Fatalf("MiB() = %d, want %d", got, size)
	}
	if got := (units.Mebibyte + units.Kibibyte).Mebibytes(); got != 1 {
		t.Fatalf("Mebibytes() = %d, want 1 (rounded down)", got)
	}
}
