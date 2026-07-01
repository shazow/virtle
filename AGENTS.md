# Agent Instructions

## Code Style

- When taking a string path input for reading/writing, convert it to an `io.Reader` or `io.Writer` in the `main` package early. Helpers and sub-packages and tests should prefer `io` interfaces.
- When designing new Go components, draw inspiration from the Go standard library and use a similar style.
- Tests: Avoid using the real filesystem in tests, prefer `io` or `io/fs` or `testing/fstest` when possible for virtual filesystem (such as `fs.File`, `fs.FS`, `fstest.MapFS`).
