package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Types covering every shape that makes jsonschema-go emit a boolean schema.
type portableNested struct {
	Payload any            `json:"payload"`
	Extra   map[string]any `json:"extra,omitempty"`
}

type portableIn struct {
	Any    any            `json:"any"`
	Map    map[string]any `json:"map"`
	Nested portableNested `json:"nested"`
	Slice  []any          `json:"slice"`
	Fixed  [3]string      `json:"fixed"`
	Pair   [2]any         `json:"pair"`
	Name   string         `json:"name"`
}

type portableOut struct {
	Result any            `json:"result"`
	Meta   map[string]any `json:"meta"`
	Items  []any          `json:"items"`
	Nested portableNested `json:"nested"`
	Fixed  [2]any         `json:"fixed"`
}

const portableToolFloor = 4

func portableHandler(_ context.Context, _ *mcp.CallToolRequest, _ portableIn) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}

func mustOutputSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	s, err := jsonschema.For[portableOut](nil)
	if err != nil {
		t.Fatalf("jsonschema.For[portableOut]: %v", err)
	}
	return s
}

// listToolsJSON connects an in-memory client and returns tool name -> the
// serialized tool exactly as it went over the wire to the client.
func listToolsJSON(t *testing.T, server *mcp.Server) map[string]map[string]any {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "portable-test", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := make(map[string]map[string]any, len(res.Tools))
	for _, tool := range res.Tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal tool %s: %v", tool.Name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal tool %s: %v", tool.Name, err)
		}
		out[tool.Name] = m
	}
	return out
}

// Keys whose value is a bag of named things: the children's keys are
// property/definition names, not JSON Schema keywords.
var nameMapKeys = map[string]bool{
	kwProperties: true, kwPatternPrp: true, "$defs": true,
	"definitions": true, kwDependent: true, "dependencies": true,
}

// Keys that hold instance data or boolean annotations rather than subschemas.
var dataKeys = map[string]bool{"enum": true, "const": true, "default": true, "examples": true}
var boolAnnotations = map[string]bool{"uniqueItems": true, "readOnly": true, "writeOnly": true, "deprecated": true}

// boolViolations walks a decoded schema and returns the JSON path of every
// boolean that is not `false` under additionalProperties/unevaluatedProperties
// and not a known boolean annotation. Deliberately generic (any boolean
// anywhere is a violation) so it does not share the normalizer's keyword list.
func boolViolations(v any, path string, namesHere bool, out *[]string) {
	switch x := v.(type) {
	case bool:
		*out = append(*out, path)
	case map[string]any:
		for k, child := range x {
			p := path + "." + k
			if !namesHere && dataKeys[k] {
				continue
			}
			if b, isBool := child.(bool); isBool && !namesHere {
				last := k
				if (last == "additionalProperties" || last == "unevaluatedProperties") && !b {
					continue
				}
				if boolAnnotations[last] {
					continue
				}
			}
			boolViolations(child, p, !namesHere && nameMapKeys[k], out)
		}
	case []any:
		for i, child := range x {
			boolViolations(child, fmt.Sprintf("%s[%d]", path, i), false, out)
		}
	}
}

func assertNoBooleanSchemas(t *testing.T, tools map[string]map[string]any) {
	t.Helper()
	if len(tools) < portableToolFloor {
		t.Fatalf("tools/list returned %d tools, want >= %d", len(tools), portableToolFloor)
	}
	for name, tool := range tools {
		for _, field := range []string{"inputSchema", "outputSchema"} {
			var bad []string
			boolViolations(tool[field], name+"."+field, false, &bad)
			for _, p := range bad {
				t.Errorf("boolean subschema at %s", p)
			}
		}
	}
}

func TestAddTool_ToolsListHasNoBooleanSubschemas(t *testing.T) {
	server := NewServer(&mcp.Implementation{Name: "p", Version: "0.0.1"}, Config{Name: "p", Version: "0.0.1"})
	out := mustOutputSchema(t)

	// Four tools through the lenient AddTool: with and without an output schema,
	// plus one whose OutputSchema is a plain map carrying a boolean.
	AddTool(server, &mcp.Tool{Name: "in_only"}, portableHandler)
	AddTool(server, &mcp.Tool{Name: "in_out", OutputSchema: out}, portableHandler)
	AddTool(server, &mcp.Tool{Name: "map_out", OutputSchema: map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"result": true, "list": map[string]any{"type": "array", "items": true}},
		"additionalProperties": true,
	}}, portableHandler)
	AddTool(server, &mcp.Tool{Name: "in_out_second", OutputSchema: out}, portableHandler)

	assertNoBooleanSchemas(t, listToolsJSON(t, server))
}

// Tools registered with the SDK's own mcp.AddTool (typed Out => inferred
// output schema) bypass this package's AddTool; the Run/Build middleware
// must cover them.
func TestToolsListMiddleware_CoversSDKRegisteredTools(t *testing.T) {
	// Precondition: the raw SDK schema really contains booleans, otherwise
	// this test proves nothing.
	raw, err := jsonschema.For[portableOut](nil)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(raw)
	var rawDoc any
	_ = json.Unmarshal(rawJSON, &rawDoc)
	var pre []string
	boolViolations(rawDoc, "raw", false, &pre)
	if len(pre) == 0 {
		t.Fatalf("precondition: inferred schema has no boolean subschema: %s", rawJSON)
	}

	server := NewServer(&mcp.Implementation{Name: "p", Version: "0.0.1"}, Config{Name: "p", Version: "0.0.1"})
	for _, name := range []string{"sdk_a", "sdk_b", "sdk_c", "sdk_d"} {
		mcp.AddTool(server, &mcp.Tool{Name: name},
			func(_ context.Context, _ *mcp.CallToolRequest, _ portableIn) (*mcp.CallToolResult, portableOut, error) {
				return nil, portableOut{}, nil
			})
	}
	applyMCPMiddleware(server, Config{})
	assertNoBooleanSchemas(t, listToolsJSON(t, server))
}

func TestPortableSchema_Rules(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"true property", `{"type":"object","properties":{"x":true}}`, `{"type":"object","properties":{"x":{}}}`},
		{"false property", `{"properties":{"x":false}}`, `{"properties":{"x":{"not":{}}}}`},
		{"additionalProperties true dropped", `{"type":"object","additionalProperties":true}`, `{"type":"object"}`},
		{"additionalProperties false kept", `{"type":"object","additionalProperties":false}`, `{"type":"object","additionalProperties":false}`},
		{"unevaluatedProperties false kept", `{"unevaluatedProperties":false}`, `{"unevaluatedProperties":false}`},
		{"unevaluatedProperties true dropped", `{"unevaluatedProperties":true}`, `{}`},
		{"additionalProperties schema recursed", `{"additionalProperties":{"properties":{"a":true}}}`, `{"additionalProperties":{"properties":{"a":{}}}}`},
		{"items single", `{"type":"array","items":true}`, `{"type":"array","items":{}}`},
		{"items array", `{"items":[true,{"type":"string"}]}`, `{"items":[{},{"type":"string"}]}`},
		{"prefixItems", `{"prefixItems":[true,{"type":"string"}]}`, `{"prefixItems":[{},{"type":"string"}]}`},
		{"additionalItems", `{"additionalItems":true}`, `{}`},
		{"patternProperties", `{"patternProperties":{"^a":true}}`, `{"patternProperties":{"^a":{}}}`},
		{"$defs", `{"$defs":{"d":true}}`, `{"$defs":{"d":{}}}`},
		{"definitions", `{"definitions":{"d":true}}`, `{"definitions":{"d":{}}}`},
		{"dependentSchemas", `{"dependentSchemas":{"a":true}}`, `{"dependentSchemas":{"a":{}}}`},
		{"dependencies schema and names", `{"dependencies":{"a":true,"b":["c"]}}`, `{"dependencies":{"a":{},"b":["c"]}}`},
		{"allOf anyOf oneOf", `{"allOf":[true],"anyOf":[true],"oneOf":[true]}`, `{"allOf":[{}],"anyOf":[{}],"oneOf":[{}]}`},
		{"not if then else", `{"not":true,"if":true,"then":true,"else":true}`, `{"not":{},"if":{},"then":{},"else":{}}`},
		{"contains propertyNames", `{"contains":true,"propertyNames":true}`, `{"contains":{},"propertyNames":{}}`},
		{"property named like a keyword", `{"properties":{"items":true,"additionalProperties":true}}`, `{"properties":{"items":{},"additionalProperties":{}}}`},
		{"enum const default untouched", `{"enum":[true,false],"const":true,"default":false,"uniqueItems":true}`, `{"enum":[true,false],"const":true,"default":false,"uniqueItems":true}`},
		{"deep nest", `{"properties":{"a":{"items":{"properties":{"b":true}}}}}`, `{"properties":{"a":{"items":{"properties":{"b":{}}}}}}`},
		{"big number preserved", `{"maximum":9007199254740993}`, `{"maximum":9007199254740993}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Feed the normalizer a RawMessage so numbers stay byte-exact.
			got := portableSchema(json.RawMessage(tc.in))
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if want := canonJSON(t, tc.want); string(gotJSON) != want {
				t.Errorf("portableSchema(%s)\n got  %s\n want %s", tc.in, gotJSON, tc.want)
			}
		})
	}
}

// canonJSON re-encodes s with sorted keys and byte-exact numbers, the same
// shape portableSchema output marshals to.
func canonJSON(t *testing.T, s string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPortableSchema_NilAndInputNotMutated(t *testing.T) {
	if got := portableSchema(nil); got != nil {
		t.Errorf("portableSchema(nil) = %v, want nil", got)
	}
	orig := map[string]any{"type": "object", "properties": map[string]any{"x": true}}
	_ = portableSchema(orig)
	if orig["properties"].(map[string]any)["x"] != true {
		t.Error("portableSchema mutated its input")
	}
}

// Validation must not change: for representative arguments the original typed
// schema and the normalized (advertised) schema accept/reject identically.
func TestPortableSchema_ValidationEquivalent(t *testing.T) {
	check := func(t *testing.T, orig *jsonschema.Schema, samples []string) {
		t.Helper()
		origR, err := orig.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
		if err != nil {
			t.Fatalf("resolve original: %v", err)
		}
		var norm jsonschema.Schema
		nj, _ := json.Marshal(portableSchema(orig))
		if err := json.Unmarshal(nj, &norm); err != nil {
			t.Fatalf("unmarshal normalized %s: %v", nj, err)
		}
		normR, err := norm.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
		if err != nil {
			t.Fatalf("resolve normalized: %v", err)
		}
		accepted, rejected := 0, 0
		for _, s := range samples {
			var a, b map[string]any
			if err := json.Unmarshal([]byte(s), &a); err != nil {
				t.Fatalf("bad sample %s: %v", s, err)
			}
			_ = json.Unmarshal([]byte(s), &b)
			e1, e2 := origR.Validate(&a), normR.Validate(&b)
			if (e1 == nil) != (e2 == nil) {
				t.Errorf("sample %s: original err=%v, normalized err=%v", s, e1, e2)
			}
			if e1 == nil {
				accepted++
			} else {
				rejected++
			}
		}
		if accepted == 0 || rejected == 0 {
			t.Errorf("samples must exercise both outcomes, accepted=%d rejected=%d", accepted, rejected)
		}
	}

	in, err := jsonschema.For[portableIn](nil)
	if err != nil {
		t.Fatal(err)
	}
	check(t, in, []string{
		`{"any":1,"map":{},"nested":{"payload":null},"slice":[],"fixed":["a","b","c"],"pair":[1,"x"],"name":"n"}`,
		`{"any":"s","map":{"k":[1,2],"z":{"y":null}},"nested":{"payload":{"a":1},"extra":{"q":true}},"slice":[1,"a",null,{}],"fixed":["a","b","c"],"pair":[null,{}],"name":"n"}`,
		`{"any":[1],"map":{"a":1},"nested":{"payload":false},"slice":[[1]],"fixed":["a","b","c"],"pair":[1,2],"name":"n"}`,
		`{"any":1,"map":"notamap","nested":{"payload":1},"slice":[],"fixed":["a","b","c"],"pair":[1,2],"name":"n"}`,
		`{"any":1,"map":{},"nested":{"payload":1},"slice":"notalist","fixed":["a","b","c"],"pair":[1,2],"name":"n"}`,
		`{"any":1,"map":{},"nested":{"payload":1},"slice":[],"fixed":["a","b"],"pair":[1,2],"name":"n"}`,
		`{"any":1,"map":{},"nested":{"payload":1},"slice":[],"fixed":["a","b","c"],"pair":[1],"name":"n"}`,
		`{"any":1,"map":{},"nested":{"payload":1},"slice":[],"fixed":["a","b","c"],"pair":[1,2],"name":5}`,
		`{"map":{},"nested":{"payload":1},"slice":[],"fixed":["a","b","c"],"pair":[1,2],"name":"n"}`,
	})

	out := mustOutputSchema(t)
	check(t, out, []string{
		`{"result":1,"meta":{"a":null},"items":[1,"x"],"nested":{"payload":2},"fixed":[1,2]}`,
		`{"result":null,"meta":{},"items":[],"nested":{"payload":{}},"fixed":[null,"a"]}`,
		`{"result":1,"meta":[],"items":[],"nested":{"payload":2},"fixed":[1,2]}`,
		`{"result":1,"meta":{},"items":{},"nested":{"payload":2},"fixed":[1,2]}`,
		`{"result":1,"meta":{},"items":[],"nested":{"payload":2},"fixed":[1]}`,
	})
}
