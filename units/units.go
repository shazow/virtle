package units

import (
	"fmt"
	"math"
)

// Bytes is a byte count. Sizes are written by scaling one of the multiplier
// constants, the way time.Duration values are written:
//
//	spec.Memory = 2048 * units.Mebibyte
type Bytes int64

// Byte-count multipliers. Sizes in virtle are binary, matching QEMU and the
// Linux tools that report guest memory.
const (
	Byte     Bytes = 1
	Kibibyte       = 1024 * Byte
	Mebibyte       = 1024 * Kibibyte
	Gibibyte       = 1024 * Mebibyte
	Tebibyte       = 1024 * Gibibyte
)

// Int64 returns the size as a plain byte count, for interfaces that take one.
func (b Bytes) Int64() int64 { return int64(b) }

// Mebibytes returns the size in whole mebibytes, rounding down.
func (b Bytes) Mebibytes() int64 { return int64(b / Mebibyte) }

// MiB returns the size as a whole number of mebibytes, rounding down. It is
// the conversion the TOML manifest needs, where sizes are MiB-denominated.
func (b Bytes) MiB() MiB { return MiB(b / Mebibyte) }

// String formats the size with the largest binary unit that divides it
// evenly, so round sizes stay readable: 2 GiB rather than 2147483648 B.
func (b Bytes) String() string {
	for _, unit := range []struct {
		size   Bytes
		suffix string
	}{
		{Tebibyte, "TiB"},
		{Gibibyte, "GiB"},
		{Mebibyte, "MiB"},
		{Kibibyte, "KiB"},
	} {
		if b != 0 && b%unit.size == 0 {
			return fmt.Sprintf("%d %s", b/unit.size, unit.suffix)
		}
	}
	return fmt.Sprintf("%d B", int64(b))
}

// MiB stores a size in mebibytes. It exists for the manifest surfaces whose
// numbers are MiB-denominated; Bytes is the type to reach for elsewhere.
type MiB int

// Bytes converts m to a byte count.
func (m MiB) Bytes() Bytes {
	return Bytes(m) * Mebibyte
}

// Int returns m as an int.
func (m MiB) Int() int {
	return int(m)
}

// BytesToMiB converts a byte count to whole mebibytes, rejecting sizes that do
// not land on a mebibyte boundary or that overflow MiB.
func BytesToMiB(size Bytes) (MiB, error) {
	if size < 0 {
		return 0, fmt.Errorf("size %s must not be negative", size)
	}
	if size%Mebibyte != 0 {
		return 0, fmt.Errorf("size %s is not a whole number of mebibytes", size)
	}
	mebibytes := size / Mebibyte
	if mebibytes > math.MaxInt {
		return 0, fmt.Errorf("size %s overflows a mebibyte count", size)
	}
	return MiB(mebibytes), nil
}
