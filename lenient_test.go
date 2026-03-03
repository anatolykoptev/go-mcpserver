package mcpserver

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestCoerceStringTypes(t *testing.T) {
	schema := &jsonschema.Schema{
		Properties: map[string]*jsonschema.Schema{
			"enabled":  {Type: "boolean"},
			"count":    {Type: "integer"},
			"ratio":    {Type: "number"},
			"name":     {Type: "string"},
		},
	}

	tests := []struct {
		name string
		key  string
		in   any
		want any
	}{
		{"bool string true", "enabled", "true", true},
		{"bool string false", "enabled", "false", false},
		{"bool string TRUE", "enabled", "TRUE", true},
		{"bool string 1", "enabled", "1", true},
		{"bool string 0", "enabled", "0", false},
		{"bool actual true", "enabled", true, true},
		{"bool invalid", "enabled", "maybe", "maybe"},
		{"int string", "count", "42", int64(42)},
		{"int actual", "count", float64(42), float64(42)},
		{"int invalid", "count", "abc", "abc"},
		{"float string", "ratio", "3.14", 3.14},
		{"string unchanged", "name", "hello", "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{tt.key: tt.in}
			coerceStringTypes(m, schema)
			if m[tt.key] != tt.want {
				t.Errorf("got %v (%T), want %v (%T)", m[tt.key], m[tt.key], tt.want, tt.want)
			}
		})
	}
}

func TestCoerceNilSchema(t *testing.T) {
	m := map[string]any{"x": "true"}
	coerceStringTypes(m, nil)
	if m["x"] != "true" {
		t.Error("nil schema should not coerce")
	}
}

func TestCoerceNoProperties(t *testing.T) {
	m := map[string]any{"x": "true"}
	coerceStringTypes(m, &jsonschema.Schema{Type: "object"})
	if m["x"] != "true" {
		t.Error("schema without properties should not coerce")
	}
}
