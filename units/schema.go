package units

import (
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// JSONSchemaTypes returns schema hints for unit codec types. Encoded manifest
// formats use strings for Duration and bare MiB counts for MiB.
func JSONSchemaTypes() map[reflect.Type]*jsonschema.Schema {
	return map[reflect.Type]*jsonschema.Schema{
		reflect.TypeOf(Duration(0)): {Type: "string"},
		reflect.TypeOf(MiB(0)):      {Type: "integer"},
	}
}
