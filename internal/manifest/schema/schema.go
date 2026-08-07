// Package schema generates the JSON Schema for the manifest input format.
package schema

import (
	"encoding/json"
	"reflect"

	"github.com/invopop/jsonschema"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/units"
)

// Generate returns the JSON Schema for the virtle manifest input format.
func Generate() *jsonschema.Schema {
	var reflector jsonschema.Reflector
	reflector = jsonschema.Reflector{
		BaseSchemaID:               jsonschema.ID("https://shazow.github.io/virtle/manifest.schema.json"),
		Anonymous:                  true,
		ExpandedStruct:             true,
		DoNotReference:             false,
		RequiredFromJSONSchemaTags: true,
		AllowAdditionalProperties:  false,
		Mapper: func(t reflect.Type) *jsonschema.Schema {
			if t == reflect.TypeOf(units.Duration(0)) {
				// Durations decode from Go duration strings or bare numbers of
				// seconds.
				return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
					{Type: "string"},
					{Type: "number"},
				}}
			}
			if t == reflect.TypeOf(manifest.MountsInput{}) {
				// MountsInput is a tagged-union slice backed by the MountEntry interface.
				// Reflection only sees []MountEntry and would emit "items: true", so map it
				// to the same concrete mount variants accepted by the manifest decoder.
				return mountSchema(&reflector)
			}
			return nil
		},
	}
	schema := reflector.Reflect(&manifest.Document{})
	schema.ID = jsonschema.ID("https://shazow.github.io/virtle/manifest.schema.json")
	schema.Title = "Virtle manifest"
	schema.Description = "JSON Schema for the virtle manifest input format emitted by virtle."
	return schema
}

// GenerateJSON returns the indented JSON encoding of Generate.
func GenerateJSON() ([]byte, error) {
	data, err := json.MarshalIndent(Generate(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func mountSchema(reflector *jsonschema.Reflector) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "array",
		Items: &jsonschema.Schema{OneOf: []*jsonschema.Schema{
			inlineSchema(reflector, manifest.VirtioFSMountInput{}),
			inlineSchema(reflector, manifest.NinePMountInput{}),
			inlineSchema(reflector, manifest.ImageMountInput{}),
		}},
	}
}

func inlineSchema(reflector *jsonschema.Reflector, value any) *jsonschema.Schema {
	schema := reflector.Reflect(value)
	schema.Version = ""
	schema.ID = ""
	return schema
}
