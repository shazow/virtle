// Package tagged decodes and encodes JSON-style tagged union lists.
//
// A tagged list is an array of objects where each object has a string "type"
// discriminator. Callers provide a Registry that maps discriminator values to
// concrete Go structs. The package is intended for manifest input structs that
// need strict per-variant decoding while preserving list order.
package tagged

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Registry maps discriminator values to variant decoders for a tagged list.
type Registry[T any] []Variant[T]

// Variant describes one concrete type in a tagged list.
type Variant[T any] struct {
	name   string
	decode func([]byte) (T, error)
}

// Value returns a Variant that decodes into V and returns it as T.
//
// V should be a concrete manifest input struct. T is usually an interface
// implemented by all variants in the list.
func Value[T any, V any](name string) Variant[T] {
	return Variant[T]{
		name: name,
		decode: func(data []byte) (T, error) {
			var target V
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&target); err != nil {
				var zero T
				return zero, err
			}
			value, ok := any(target).(T)
			if !ok {
				var zero T
				return zero, fmt.Errorf("decoded %T does not implement registry target type", target)
			}
			return value, nil
		},
	}
}

// DecodeJSONList decodes a JSON array of tagged objects using registry.
//
// The path parameter is used in validation errors, for example
// "manifest.mounts".
func DecodeJSONList[T any](data []byte, path string, registry Registry[T]) ([]T, error) {
	var rawValues []json.RawMessage
	if err := json.Unmarshal(data, &rawValues); err != nil {
		return nil, err
	}
	values := make([]T, 0, len(rawValues))
	for i, raw := range rawValues {
		value, err := decodeJSONValue(raw, path, i, registry)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// DecodeTOMLList decodes a TOML array of tables using registry.
//
// BurntSushi/toml passes arrays of tables as []map[string]any and empty arrays
// as []any; both forms are accepted.
func DecodeTOMLList[T any](data any, path string, registry Registry[T]) ([]T, error) {
	rawValues, err := tomlMaps(data, path)
	if err != nil {
		return nil, err
	}
	values := make([]T, 0, len(rawValues))
	for i, raw := range rawValues {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", path, i, err)
		}
		value, err := decodeJSONValue(encoded, path, i, registry)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// MarshalJSONList encodes values as a JSON array and writes each value's type.
//
// The tag function returns the discriminator value to store in the "type"
// field for each item.
func MarshalJSONList[T any](values []T, tag func(T) string) ([]byte, error) {
	marshaled := make([]any, 0, len(values))
	for _, value := range values {
		marshaledValue, err := marshalJSONValue(value, tag)
		if err != nil {
			return nil, err
		}
		marshaled = append(marshaled, marshaledValue)
	}
	return json.Marshal(marshaled)
}

func decodeJSONValue[T any](data []byte, path string, index int, registry Registry[T]) (T, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		var zero T
		return zero, fmt.Errorf("%s[%d]: %w", path, index, err)
	}
	variant, ok := registry.lookup(header.Type)
	if !ok {
		var zero T
		return zero, fmt.Errorf("%s[%d].type must be %s", path, index, registry.expectedTypes())
	}
	value, err := variant.decode(data)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s[%d]: %w", path, index, err)
	}
	return value, nil
}

func marshalJSONValue[T any](value T, tag func(T) string) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	result["type"] = tag(value)
	return result, nil
}

func tomlMaps(data any, path string) ([]map[string]any, error) {
	switch values := data.(type) {
	case []map[string]any:
		return values, nil
	case []any:
		rawValues := make([]map[string]any, 0, len(values))
		for i, value := range values {
			raw, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be a table", path, i)
			}
			rawValues = append(rawValues, raw)
		}
		return rawValues, nil
	default:
		return nil, fmt.Errorf("%s must be an array of tables", path)
	}
}

func (r Registry[T]) lookup(name string) (Variant[T], bool) {
	for _, variant := range r {
		if variant.name == name {
			return variant, true
		}
	}
	return Variant[T]{}, false
}

func (r Registry[T]) expectedTypes() string {
	names := make([]string, 0, len(r))
	for _, variant := range r {
		names = append(names, variant.name)
	}
	switch len(names) {
	case 0:
		return "set"
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return "one of " + strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}
