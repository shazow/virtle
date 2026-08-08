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

// TestManifestSchemaValidatesDocuments exercises the schema as a validator:
// a minimal valid manifest passes and structurally invalid ones fail.
func TestManifestSchemaValidatesDocuments(t *testing.T) {
	schema, err := Generate()
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}

	valid := decodeJSON(t, `{
		"kernel": {"path": "/boot/vmlinuz", "initrd_path": "/boot/initrd"},
		"machine": {"memory": 1024},
		"qemu": {"guest_default_timeout": "30s"},
		"mounts": [{"type": "image", "source": "/tmp/root.img"}]
	}`)
	if err := resolved.Validate(valid); err != nil {
		t.Fatalf("valid manifest should pass schema validation: %v", err)
	}

	for name, document := range map[string]string{
		"missing required kernel":    `{}`,
		"missing kernel initrd_path": `{"kernel": {"path": "/boot/vmlinuz"}}`,
		"memory with wrong type":     `{"kernel": {"path": "/k", "initrd_path": "/i"}, "machine": {"memory": "lots"}}`,
		"unknown property":           `{"kernel": {"path": "/k", "initrd_path": "/i"}, "kernell": {}}`,
		"mount without type":         `{"kernel": {"path": "/k", "initrd_path": "/i"}, "mounts": [{"source": "/tmp/x.img"}]}`,
	} {
		if err := resolved.Validate(decodeJSON(t, document)); err == nil {
			t.Errorf("%s: expected schema validation to fail", name)
		}
	}
}

func decodeJSON(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode test document: %v", err)
	}
	return value
}
