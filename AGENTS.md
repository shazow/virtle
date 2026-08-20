# Agent Instructions

## Code

- When taking a string path input for reading/writing, convert it to an `io.Reader` or `io.Writer` in the `main` package early. Helpers and sub-packages and tests should prefer `io` interfaces.
- When designing new Go components, draw inspiration from the Go standard library and use a similar style.
- Tests: Avoid using the real filesystem in tests, prefer `io` or `io/fs` or `testing/fstest` when possible for virtual filesystem (such as `fs.File`, `fs.FS`, `fstest.MapFS`).
- Document substantial consumer-facing changes in `HISTORY.md`.

## Testing

- Tests should be evergreen and focus on exercising affirmative functionality.
- Name temporary tests clearly or add a short `XXX` comment with the condition for removing them.
- When writing a test to validate the removal of some functionality, comment that it is temporary. Mention the tested scenario in the validation report but remove temporary tests before the task is completed.
- Avoid using timed sleeps in tests, prefer signal-based control flows whenever possible. If sleep is the only way, then use a module-global constant to have uniform values for slow and fast sleep durations.

## Commit

- First line: `<component name>: <short description of changes>`
- Following paragraph: `<Additional details about the changes and motivation worth noting>`
- If we're completing a specific issue number, mention it: `Closes #234`
- Final line should include: `Assisted-by: HARNESS:MODEL_VERSION` (such as `Assisted-by: codex:gpt-5.6-sol`)
