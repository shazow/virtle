// Package schema generates the JSON Schema for the manifest input format.
package schema

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

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
				// Durations are documented as Go duration strings; the decoder
				// also accepts bare numbers of seconds for backward
				// compatibility, deliberately left out of the schema.
				return &jsonschema.Schema{Type: "string"}
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
	applyDefaultTags(schema, schema, reflect.TypeOf(manifest.Document{}), map[reflect.Type]bool{})
	return schema
}

// applyDefaultTags copies each field's `default` struct tag — the same tag the
// decoder seeds omitted keys from — into the generated schema's default
// keyword, so defaults are self-documenting without restating them in
// descriptions.
func applyDefaultTags(root *jsonschema.Schema, s *jsonschema.Schema, t reflect.Type, visited map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || s == nil || visited[t] {
		return
	}
	visited[t] = true
	s = resolveRef(root, s)
	if s == nil {
		return
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous {
			// Embedded fields flatten into the same property set.
			applyDefaultTags(root, s, field.Type, visited)
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" || s.Properties == nil {
			continue
		}
		property, ok := s.Properties.Get(name)
		if !ok || property == nil {
			continue
		}
		if tag, ok := field.Tag.Lookup("default"); ok {
			property.Default = defaultValue(field.Type, tag)
		}

		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer || fieldType.Kind() == reflect.Slice {
			fieldType = fieldType.Elem()
			if property != nil {
				property = property.Items
			}
		}
		applyDefaultTags(root, property, fieldType, visited)
	}
}

// resolveRef follows a local $defs reference to its definition.
func resolveRef(root *jsonschema.Schema, s *jsonschema.Schema) *jsonschema.Schema {
	if s == nil || s.Ref == "" {
		return s
	}
	name := strings.TrimPrefix(s.Ref, "#/$defs/")
	if definition, ok := root.Definitions[name]; ok {
		return definition
	}
	return nil
}

// defaultValue renders a `default` tag with the same type the schema gives the
// field: durations stay strings and numeric fields become numbers.
func defaultValue(t reflect.Type, tag string) any {
	if t == reflect.TypeOf(units.Duration(0)) {
		return tag
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(tag, 10, 64); err == nil {
			return n
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(tag, 10, 64); err == nil {
			return n
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(tag, 64); err == nil {
			return f
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(tag); err == nil {
			return b
		}
	}
	return tag
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
