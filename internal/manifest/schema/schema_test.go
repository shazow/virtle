package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestGeneratedManifestSchemaIsCurrent(t *testing.T) {
	got, err := GenerateJSON()
	if err != nil {
		t.Fatalf("read regenerated schema: %v", err)
	}
	want, err := os.ReadFile("../../../manifest.schema.json")
	if err != nil {
		t.Fatalf("read checked-in schema: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("manifest.schema.json is stale; run go run . manifest schema > manifest.schema.json from virtle/")
	}
}

func TestManifestSchemaIsValidJSON(t *testing.T) {
	data, err := os.ReadFile("../../../manifest.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if got, want := schema["title"], "Virtle manifest"; got != want {
		t.Fatalf("schema title = %v, want %q", got, want)
	}
}

// TestManifestSchemaKeepsSemanticFeatures guards the behaviors the generator
// deliberately produces; the staleness test alone accepts whatever a
// regeneration emits, so a library change could drop these silently.
func TestManifestSchemaKeepsSemanticFeatures(t *testing.T) {
	schema, err := Generate()
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}

	// Durations are documented as strings, with the decoder-seeded default.
	timeout := schema.Properties["qemu"].Properties["guest_default_timeout"]
	if timeout == nil || timeout.Type != "string" {
		t.Fatalf("guest_default_timeout should have type string, got %+v", timeout)
	}
	if got, want := string(timeout.Default), `"30s"`; got != want {
		t.Fatalf("guest_default_timeout default = %s, want %s", got, want)
	}

	// Defaults that only DefaultDocument knows about are still emitted.
	backend := schema.Properties["graphics"].Properties["backend"]
	if got, want := string(backend.Default), `"headless"`; got != want {
		t.Fatalf("graphics backend default = %s, want %s", got, want)
	}

	// Required fields the manifest validation enforces stay expressed.
	if got, want := schema.Required, []string{"kernel"}; !slices.Equal(got, want) {
		t.Fatalf("root required = %v, want %v", got, want)
	}
	kernel := schema.Properties["kernel"]
	if got, want := kernel.Required, []string{"path", "initrd_path"}; !slices.Equal(got, want) {
		t.Fatalf("kernel required = %v, want %v", got, want)
	}
	if kernel.Properties["path"].Default != nil {
		t.Fatalf("required kernel path must not carry a default, got %s", kernel.Properties["path"].Default)
	}

	// The mount tagged union keeps its three variants.
	mounts := schema.Properties["mounts"]
	if mounts.Type != "array" || mounts.Items == nil || len(mounts.Items.OneOf) != 3 {
		t.Fatalf("mounts should be an array with a three-variant oneOf, got %+v", mounts)
	}
}
