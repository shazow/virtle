// Package units defines small typed scalars shared across the virtle API,
// so sizes and durations are never plumbed around as bare ints.
package units

import "fmt"

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
