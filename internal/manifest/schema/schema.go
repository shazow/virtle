// Package schema generates the JSON Schema for the manifest input format.
package schema

import (
	"encoding/json"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/units"
)

// Generate returns the JSON Schema for the virtle manifest input format.
func Generate() (*jsonschema.Schema, error) {
	opts := &jsonschema.ForOptions{
		TypeSchemas: units.JSONSchemaTypes(),
	}

	// MountsInput is a tagged-union slice backed by the MountEntry interface.
	// Reflection only sees []MountEntry, so map it to the concrete mount
	// variants accepted by the manifest decoder.
	mounts, err := mountSchema(opts)
	if err != nil {
		return nil, err
	}
	opts.TypeSchemas[reflect.TypeOf(manifest.MountsInput{})] = mounts

	schema, err := jsonschema.ForType(reflect.TypeOf(manifest.Document{}), opts)
	if err != nil {
		return nil, err
	}
	schema.Schema = "https://json-schema.org/draft/2020-12/schema"
	schema.ID = "https://shazow.github.io/virtle/manifest.schema.json"
	schema.Title = "Virtle manifest"
	schema.Description = "JSON Schema for the virtle manifest input format emitted by virtle."
	if err := applyDocumentDefaults(schema); err != nil {
		return nil, err
	}
	return schema, nil
}

// GenerateJSON returns the indented JSON encoding of Generate.
func GenerateJSON() ([]byte, error) {
	schema, err := Generate()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func mountSchema(opts *jsonschema.ForOptions) (*jsonschema.Schema, error) {
	var variants []*jsonschema.Schema
	for _, variant := range []any{
		manifest.VirtioFSMountInput{},
		manifest.NinePMountInput{},
		manifest.ImageMountInput{},
	} {
		schema, err := jsonschema.ForType(reflect.TypeOf(variant), opts)
		if err != nil {
			return nil, err
		}
		variants = append(variants, schema)
	}
	return &jsonschema.Schema{
		Type:  "array",
		Items: &jsonschema.Schema{OneOf: variants},
	}, nil
}

// applyDocumentDefaults copies DefaultDocument's non-zero values into the
// schema's default keywords, so the schema self-documents what an omitted key
// resolves to — including the defaults the decoder seeds from struct tags.
func applyDocumentDefaults(s *jsonschema.Schema) error {
	data, err := json.Marshal(manifest.DefaultDocument())
	if err != nil {
		return err
	}
	var defaults map[string]any
	if err := json.Unmarshal(data, &defaults); err != nil {
		return err
	}
	setDefaults(s, defaults)
	return nil
}

func setDefaults(s *jsonschema.Schema, defaults map[string]any) {
	if s == nil {
		return
	}
	for name, value := range defaults {
		property := s.Properties[name]
		if property == nil {
			continue
		}
		if object, ok := value.(map[string]any); ok {
			setDefaults(property, object)
			continue
		}
		// Required fields carry no omitempty, so unset ones marshal as zero
		// values (kernel.path as ""); those are absences, not defaults.
		if value == nil || value == "" {
			continue
		}
		if raw, err := json.Marshal(value); err == nil {
			property.Default = raw
		}
	}
}
