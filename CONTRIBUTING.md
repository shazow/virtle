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

To cut a release, tag the tip of `main` `vX.Y.Z` and push the tag. The release
workflow runs `scripts/update-release-nix X.Y.Z`, commits the `release.nix`
change to `main`, moves the tag onto that commit, publishes from it, and moves
`main` back to `X.Y.Z-dev` afterwards. A tag that is not on the tip of `main`
must carry a matching `release.nix` version already, or the workflow rejects
it.

## Pull Request
- Mention any relevant issues, especially if the pull request fixes them. (Example: "Fixes #123")
