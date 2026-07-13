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
