package llm

import (
	"reflect"
	"strconv"
	"strings"
)

// GenerateSchema builds a JSON-Schema object (as a map) describing t, for use as
// a tool's parameter schema or a structured-output schema. It is intentionally
// small — it covers the shapes toroid's tools actually use: structs with string/
// bool/number/integer fields, slices, maps, nested structs, and pointers. Field
// names come from json tags; a field is required unless its json tag has
// ",omitempty" or the field is a pointer. A `description` struct tag becomes the
// property description.
//
// This replaces the third-party reflection schema generator so the module owns
// its whole tool pipeline.
func GenerateSchema(t reflect.Type) map[string]any {
	return schemaForType(t)
}

func schemaForType(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaForType(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaForType(t.Elem())}
	case reflect.Struct:
		return schemaForStruct(t)
	case reflect.Interface:
		return map[string]any{} // any
	default:
		return map[string]any{"type": "string"}
	}
}

func schemaForStruct(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name, omitempty, skip := jsonFieldName(f)
		if skip {
			continue
		}
		fieldSchema := schemaForType(f.Type)
		if desc := fieldDescription(f); desc != "" {
			fieldSchema["description"] = desc
		}
		applySchemaTags(fieldSchema, f.Tag.Get("jsonschema"))
		props[name] = fieldSchema
		if !omitempty && f.Type.Kind() != reflect.Ptr {
			required = append(required, name)
		}
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func applySchemaTags(schema map[string]any, tag string) {
	for _, seg := range strings.Split(tag, ",") {
		key, value, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		switch key {
		case "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems":
			if n, err := strconv.Atoi(value); err == nil {
				schema[key] = n
			}
		}
	}
}

// fieldDescription resolves a field's schema description from either a plain
// `description:"…"` tag or the `jsonschema:"description=…,…"` convention the
// existing tool arg structs use. In the jsonschema form, the description runs
// from "description=" until the next ",key=" segment, so descriptions may
// contain bare commas.
func fieldDescription(f reflect.StructField) string {
	if d := f.Tag.Get("description"); d != "" {
		return d
	}
	tag := f.Tag.Get("jsonschema")
	if tag == "" {
		return ""
	}
	segs := strings.Split(tag, ",")
	desc := ""
	collecting := false
	for _, seg := range segs {
		switch {
		case strings.HasPrefix(seg, "description="):
			desc = strings.TrimPrefix(seg, "description=")
			collecting = true
		case collecting && !strings.Contains(seg, "="):
			desc += "," + seg // a comma inside the description text
		default:
			collecting = false
		}
	}
	return desc
}

// jsonFieldName resolves a struct field's JSON name and omitempty flag.
func jsonFieldName(f reflect.StructField) (name string, omitempty, skip bool) {
	if f.Tag.Get("jsonschema") == "-" {
		return "", false, true
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = f.Name
	if parts[0] != "" {
		name = parts[0]
	}
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}
