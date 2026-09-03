// Package units defines small typed scalars shared across the virtle API,
// so sizes and durations are never plumbed around as bare ints.
package units

import (
	"encoding/json"
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

// MiB is a MiB-denominated codec value used by encoded formats whose bare
// numbers count mebibytes. Use Bytes for API parameters and calculations.
type MiB int

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

// Bytes converts a MiB-denominated value to bytes.
func (m MiB) Bytes() Bytes { return Bytes(m) * Mebibyte }

// Int returns m as a plain int count of mebibytes.
func (m MiB) Int() int { return int(m) }

// Kibibytes returns b as a whole number of kibibytes, truncating toward zero.
func (b Bytes) Kibibytes() int64 { return int64(b / Kibibyte) }

// Mebibytes returns b as a whole number of mebibytes, truncating toward
// zero. Useful at boundaries (such as the manifest format) that are
// MiB-denominated.
func (b Bytes) Mebibytes() MiB { return MiB(b / Mebibyte) }

// Gibibytes returns b as a whole number of gibibytes, truncating toward zero.
func (b Bytes) Gibibytes() int64 { return int64(b / Gibibyte) }

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

// ParseBytes parses an integer followed by B, KiB, MiB, GiB, or TiB. The
// grammar is case-sensitive and permits no whitespace (for example, "2GiB").
func ParseBytes(value string) (Bytes, error) {
	for _, u := range []struct {
		suffix string
		unit   Bytes
	}{
		{"TiB", Tebibyte},
		{"GiB", Gibibyte},
		{"MiB", Mebibyte},
		{"KiB", Kibibyte},
		{"B", Byte},
	} {
		if !strings.HasSuffix(value, u.suffix) {
			continue
		}
		raw := strings.TrimSuffix(value, u.suffix)
		count, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || raw == "" {
			break
		}
		if count > math.MaxInt64/int64(u.unit) || count < math.MinInt64/int64(u.unit) {
			return 0, fmt.Errorf("invalid byte size %q: overflows int64", value)
		}
		return Bytes(count) * u.unit, nil
	}
	return 0, fmt.Errorf("invalid byte size %q: expected an integer with B, KiB, MiB, GiB, or TiB suffix", value)
}

// MarshalText encodes b using its canonical unit-suffixed string form.
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText decodes the unit-suffixed grammar accepted by ParseBytes.
func (b *Bytes) UnmarshalText(text []byte) error {
	parsed, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// MarshalJSON encodes the size as a unit-suffixed JSON string, like
// MarshalText.
func (b Bytes) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

// UnmarshalJSON accepts a unit-suffixed JSON string such as "2GiB".
func (b *Bytes) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("byte size must be a string such as %q", "2GiB")
	}
	return b.UnmarshalText([]byte(value))
}

// UnmarshalTOML decodes a unit-suffixed byte-size string.
func (b *Bytes) UnmarshalTOML(value any) error {
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("byte size must be a string such as %q, got %T", "2GiB", value)
	}
	return b.UnmarshalText([]byte(text))
}
