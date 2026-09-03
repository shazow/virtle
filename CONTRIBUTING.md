# Contributing Guidelines

## Motivation: Are you a first time contributor?
- We are not looking for drive-by or fully automated contributions.
- Please contribute if you are actively using virtle and would like to improve a feature that you rely on.
- Include some details of how you're using virtle and the impact of this change on you personally.

## Development

CI runs the following on every pull request; run them locally before pushing:

```console
$ gofmt -l .
$ go mod tidy -diff
$ go vet -tags integration ./...
$ go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 -tags integration ./...
$ go test -race -shuffle=on ./...
$ nix build .#virtle --no-link
$ nix flake check
```

`nix flake check` runs the `integration`-tagged tests inside a small VM. After
changing `go.mod` or `go.sum`, run `scripts/update-release-nix` (with no
argument it keeps the current version) to refresh the vendor hash in
`release.nix`; otherwise the Nix job fails.

To cut a release, run `scripts/update-release-nix X.Y.Z`, commit the
`release.nix` change, and tag that commit `vX.Y.Z`. The release workflow
rejects tags whose `release.nix` version does not match, and moves `main` back
to `X.Y.Z-dev` afterwards.

## Pull Request
- Mention any relevant issues, especially if the pull request fixes them. (Example: "Fixes #123")
