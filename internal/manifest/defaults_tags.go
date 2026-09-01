package manifest

import (
	"fmt"
	"reflect"
	"strconv"

	"github.com/shazow/virtle/units"
)

var durationType = reflect.TypeOf(units.Duration(0))

// applyDefaultTags sets each zero scalar field to the value of its `default`
// struct tag, recursing through nested structs. Decoding runs after it, so an
// omitted key keeps the default while an explicitly zero value survives.
func applyDefaultTags(doc *Document) {
	applyDefaultTagsValue(reflect.ValueOf(doc).Elem())
}

func applyDefaultTagsValue(v reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)
		tag, ok := field.Tag.Lookup("default")
		if !ok {
			if fv.Kind() == reflect.Struct {
				applyDefaultTagsValue(fv)
			}
			continue
		}
		if !fv.IsZero() {
			continue
		}
		if field.Type == durationType {
			value, err := units.ParseDuration(tag)
			if err != nil {
				panic(fmt.Sprintf("manifest: field %s has invalid default tag %q: %v", field.Name, tag, err))
			}
			fv.SetInt(int64(value))
			continue
		}
		switch fv.Kind() {
		case reflect.Float64:
			value, err := strconv.ParseFloat(tag, 64)
			if err != nil {
				panic(fmt.Sprintf("manifest: field %s has invalid default tag %q: %v", field.Name, tag, err))
			}
			fv.SetFloat(value)
		case reflect.String:
			fv.SetString(tag)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			value, err := strconv.ParseInt(tag, 10, 64)
			if err != nil {
				panic(fmt.Sprintf("manifest: field %s has invalid default tag %q: %v", field.Name, tag, err))
			}
			fv.SetInt(value)
		default:
			panic(fmt.Sprintf("manifest: field %s has default tag on unsupported kind %s", field.Name, fv.Kind()))
		}
	}
}
