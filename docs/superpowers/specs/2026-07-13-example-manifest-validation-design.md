# Example Manifest Validation Test

Add a root-package test in `examples_test.go` that discovers every
`examples/*.toml` file and validates it through `internal/manifest.Load`.

The test will fail when the glob matches no files, then run each manifest as a
named subtest. Each file will be opened as an `io.Reader`, passed to the real
manifest parsing, defaulting, resolution, and validation path, and closed
within its subtest. An invalid example or an unreadable file will report its
path in the test failure.

Verification will temporarily add an invalid matching example to demonstrate
that the test fails for the intended reason, remove that temporary file, and
then run the focused test and the full Go test suite.
