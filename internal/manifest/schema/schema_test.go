package schema

import (
	"bytes"
	"encoding/json"
	"os"
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
