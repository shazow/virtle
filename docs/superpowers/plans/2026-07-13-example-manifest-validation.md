# Example Manifest Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a short root-level test that keeps every `examples/*.toml` manifest valid.

**Architecture:** A root-package test uses `os.DirFS` and `fs.Glob` to discover all TOML examples, then opens each file through `fs.FS` and passes its `io.Reader` to the real `manifest.Load` validation path. A temporary invalid matching file proves the test detects regressions before the fixture is removed.

**Tech Stack:** Go standard library (`io/fs`, `os`, `testing`) and `internal/manifest`.

---

### Task 1: Validate all example manifests

**Files:**
- Create: `examples_test.go`
- Temporarily create and remove: `examples/invalid-test.toml`
- Modify: `docs/superpowers/plans/2026-07-13-example-manifest-validation.md`

- [x] **Step 1: Add the test and a deliberately invalid example**

Create `examples_test.go`:

```go
package main

import (
	"io/fs"
	"os"
	"testing"

	"github.com/shazow/virtle/internal/manifest"
)

func TestExampleManifests(t *testing.T) {
	examples := os.DirFS("examples")
	names, err := fs.Glob(examples, "*.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no example manifests found")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file, err := examples.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			if _, err := manifest.Load(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}
```

Temporarily create `examples/invalid-test.toml`:

```toml
[kernel]
path = "/tmp/vmlinuz"
```

- [x] **Step 2: Run the focused test and verify the intended failure**

Run: `go test . -run '^TestExampleManifests$'`

Expected: FAIL in `invalid-test.toml` because `manifest.kernel.initrd_path` is required.

- [x] **Step 3: Remove the temporary invalid example**

Delete `examples/invalid-test.toml`; it is mutation-check scaffolding and must not be committed.

- [x] **Step 4: Format and run the focused test**

Run: `gofmt -w examples_test.go && go test . -run '^TestExampleManifests$'`

Expected: PASS for every discovered TOML example.

- [x] **Step 5: Run the full test suite**

Run: `go test ./...`

Expected: all packages PASS.

- [x] **Step 6: Verify scope and commit**

Run: `git diff --check && git status --short`

Expected: the root test and this plan are changed, `examples/invalid-test.toml` is absent, and unrelated working-tree changes remain unstaged.

```bash
git add examples_test.go docs/superpowers/plans/2026-07-13-example-manifest-validation.md
git commit -m "test: validate example manifests"
```
