// Package units defines byte sizes shared across the virtle API.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Bytes is a size in bytes. Sizes are expressed by multiplying the unit
// constants, mirroring time.Duration:
//
//	mem := 2048 * units.Mebibyte
type Bytes int64

// Binary (IEC) size units.
const (
	Byte     Bytes = 1
	Kibibyte       = 1024 * Byte
	Mebibyte       = 1024 * Kibibyte
	Gibibyte       = 1024 * Mebibyte
	Tebibyte       = 1024 * Gibibyte
)

// Int64 returns b as a plain int64 count of bytes.
func (b Bytes) Int64() int64 { return int64(b) }

// Mebibytes returns b as a whole number of mebibytes, truncating toward
// zero. Useful at boundaries (such as the manifest format) that are
// MiB-denominated.
func (b Bytes) Mebibytes() int64 { return int64(b / Mebibyte) }

// String formats b using the largest unit that divides it evenly, e.g.
// "2GiB", "512MiB", "1536B".
func (b Bytes) String() string {
	units := []struct {
		unit   Bytes
		suffix string
	}{
		{Tebibyte, "TiB"},
		{Gibibyte, "GiB"},
		{Mebibyte, "MiB"},
		{Kibibyte, "KiB"},
	}
	for _, u := range units {
		if b != 0 && b%u.unit == 0 {
			return fmt.Sprintf("%d%s", b/u.unit, u.suffix)
		}
	}
	return fmt.Sprintf("%dB", int64(b))
}

// ParseBytes parses an IEC byte size such as "2GiB", "512MiB", or
// "1536B".
func ParseBytes(s string) (Bytes, error) {
	units := []struct {
		suffix string
		unit   Bytes
	}{
		{"TiB", Tebibyte},
		{"GiB", Gibibyte},
		{"MiB", Mebibyte},
		{"KiB", Kibibyte},
		{"B", Byte},
	}
	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid byte size %q", s)
		}
		if value > math.MaxInt64/int64(u.unit) {
			return 0, fmt.Errorf("byte size %q overflows int64", s)
		}
		return Bytes(value * int64(u.unit)), nil
	}
	return 0, fmt.Errorf("invalid byte size %q: expected an IEC size such as %q", s, "512MiB")
}

// MarshalText encodes b in the form accepted by ParseBytes.
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText decodes an IEC byte size.
func (b *Bytes) UnmarshalText(text []byte) error {
	parsed, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}
