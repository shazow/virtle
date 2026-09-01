// Package units defines the small typed scalars shared across the virtle
// API, so sizes are never plumbed around as bare ints. Durations are
// time.Duration throughout the API.
package units

import (
	"fmt"
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

// ParseBytes parses a size in the form String produces: an integer
// followed by an optional IEC unit suffix, e.g. "2GiB", "512MiB", "1536B".
// A bare integer is a count of bytes.
func ParseBytes(s string) (Bytes, error) {
	digits := strings.TrimRight(s, "BKMGTi")
	suffix := s[len(digits):]
	unit, ok := byteUnits[suffix]
	if !ok || digits == "" {
		return 0, fmt.Errorf("invalid size %q: expected a size such as %q", s, "512MiB")
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q: expected a size such as %q", s, "512MiB")
	}
	if unit != Byte && n > int64(maxBytes/unit) {
		return 0, fmt.Errorf("invalid size %q: overflows", s)
	}
	return Bytes(n) * unit, nil
}

// MarshalText encodes b in the form String produces, so text-based
// encoders (TOML, JSON) round-trip sizes as "2GiB".
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText decodes the forms ParseBytes accepts.
func (b *Bytes) UnmarshalText(text []byte) error {
	parsed, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

const maxBytes = Bytes(1<<63 - 1)

var byteUnits = map[string]Bytes{
	"":    Byte,
	"B":   Byte,
	"KiB": Kibibyte,
	"MiB": Mebibyte,
	"GiB": Gibibyte,
	"TiB": Tebibyte,
}

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
