// Package units defines the typed scalars virtle uses instead of bare
// numbers, so a size or a delay cannot be plumbed through the API as an
// ambiguous int.
//
// Sizes are [Bytes], written by scaling one of the multiplier constants the
// way a time.Duration is written:
//
//	spec.Memory = 2048 * units.Mebibyte
//
// [MiB] remains for the manifest surfaces whose numbers are MiB-denominated,
// and [Duration] decodes Go duration strings from TOML and JSON.
package units
