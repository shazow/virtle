package units

const bytesPerMiB int64 = 1024 * 1024

// MiB stores a size in mebibytes.
type MiB int

// Bytes converts m to bytes.
func (m MiB) Bytes() int64 {
	return int64(m) * bytesPerMiB
}

// Int returns m as an int.
func (m MiB) Int() int {
	return int(m)
}
