// Package schema generates the JSON Schema for the manifest input format.
package schema

import (
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shazow/virtle/internal/manifest"
	"github.com/shazow/virtle/internal/units"
)

// Generate returns the JSON Schema for the virtle manifest input format.
func Generate() (*jsonschema.Schema, error) {
	opts := &jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			// Durations are documented as Go duration strings; the decoder
			// also accepts bare numbers of seconds for backward
			// compatibility, deliberately left out of the schema.
			reflect.TypeOf(units.Duration(0)): {Type: "string"},
		},
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
	if err := applyDefaultTags(schema, reflect.TypeOf(manifest.Document{})); err != nil {
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

// applyDefaultTags copies each field's `default` struct tag — the same tag the
// decoder seeds omitted keys from — into the generated schema's default
// keyword, so defaults are self-documenting without restating them in
// descriptions.
func applyDefaultTags(s *jsonschema.Schema, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s == nil || t.Kind() != reflect.Struct {
		return nil
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous {
			// Embedded fields flatten into the same property set.
			if err := applyDefaultTags(s, field.Type); err != nil {
				return err
			}
			continue
		}
		property := s.Properties[jsonName(field)]
		if property == nil {
			continue
		}
		if tag, ok := field.Tag.Lookup("default"); ok {
			raw, err := json.Marshal(defaultValue(field.Type, tag))
			if err != nil {
				return err
			}
			property.Default = raw
		}

		fieldType := field.Type
	deref:
		for property != nil {
			switch fieldType.Kind() {
			case reflect.Pointer:
				fieldType = fieldType.Elem()
			case reflect.Slice:
				fieldType = fieldType.Elem()
				property = property.Items
			default:
				break deref
			}
		}
		if err := applyDefaultTags(property, fieldType); err != nil {
			return err
		}
	}
	return nil
}

func jsonName(field reflect.StructField) string {
	name := field.Tag.Get("json")
	for i, r := range name {
		if r == ',' {
			return name[:i]
		}
	}
	return name
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
