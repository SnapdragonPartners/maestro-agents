package jsonschema

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, schema string) *Schema {
	t.Helper()
	s, err := Compile([]byte(schema))
	if err != nil {
		t.Fatalf("Compile(%s): %v", schema, err)
	}
	return s
}

func TestCompileRefuses(t *testing.T) {
	cases := map[string]string{
		"not JSON":                  `{`,
		"trailing data":             `{} {}`,
		"trailing garbage":          `{}x`,
		"trailing bracket":          `{}]`,
		"not a schema":              `3`,
		"unsupported keyword":       `{"oneOf": []}`,
		"ref":                       `{"$ref": "#/defs/x"}`,
		"format":                    `{"type": "string", "format": "email"}`,
		"nested unsupported":        `{"properties": {"a": {"anyOf": []}}}`,
		"items unsupported":         `{"items": {"not": {}}}`,
		"additional unsupported":    `{"additionalProperties": {"if": {}}}`,
		"tuple items":               `{"items": [{"type": "string"}]}`,
		"unknown type":              `{"type": "date"}`,
		"empty type list":           `{"type": []}`,
		"duplicate type":            `{"type": ["string", "string"]}`,
		"type not string":           `{"type": 3}`,
		"properties not object":     `{"properties": []}`,
		"property not schema":       `{"properties": {"a": 3}}`,
		"required not list":         `{"required": "a"}`,
		"required duplicate":        `{"required": ["a", "a"]}`,
		"required not strings":      `{"required": [1]}`,
		"enum empty":                `{"enum": []}`,
		"enum not list":             `{"enum": 3}`,
		"minimum not number":        `{"minimum": "1"}`,
		"draft 4 exclusive":         `{"exclusiveMinimum": true}`,
		"minLength negative":        `{"minLength": -1}`,
		"minLength fraction":        `{"minLength": 1.5}`,
		"maxItems huge":             `{"maxItems": 1e30}`,
		"pattern not string":        `{"pattern": 3}`,
		"pattern invalid":           `{"pattern": "("}`,
		"pattern lookahead":         `{"pattern": "(?=a)"}`,
		"additionalProperties num":  `{"additionalProperties": 3}`,
		"invalid UTF-8":             "{\"title\": \"\xff\"}",
		"patternProperties refused": `{"patternProperties": {}}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile([]byte(schema)); err == nil {
				t.Fatalf("Compile(%s) succeeded", schema)
			}
		})
	}
}

func TestCompileAcceptsAnnotations(t *testing.T) {
	mustCompile(t, `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$comment": "c",
		"title": "t", "description": "d", "default": {}, "examples": [1], "deprecated": false}`)
	mustCompile(t, `true`)
	mustCompile(t, `false`)
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name, schema string
		ok, bad      []string
	}{
		{"type single", `{"type": "string"}`, []string{`"a"`}, []string{`1`, `null`, `{}`}},
		{"type list", `{"type": ["string", "null"]}`, []string{`"a"`, `null`}, []string{`1`}},
		{"integer", `{"type": "integer"}`, []string{`1`, `1.0`, `-3`, `1e2`}, []string{`1.5`, `"1"`}},
		{"number", `{"type": "number"}`, []string{`1`, `1.5`}, []string{`true`}},
		{"boolean", `{"type": "boolean"}`, []string{`true`}, []string{`0`}},
		{"array", `{"type": "array"}`, []string{`[]`}, []string{`{}`}},
		{"object", `{"type": "object"}`, []string{`{}`}, []string{`[]`, `null`}},
		{"true schema", `true`, []string{`1`, `null`}, nil},
		{"false schema", `false`, nil, []string{`1`, `null`}},
		{"empty schema", `{}`, []string{`1`, `null`, `{"a": 1}`}, nil},
		{"enum", `{"enum": ["a", 1, null, {"x": [1]}]}`, []string{`"a"`, `1.0`, `null`, `{"x": [1]}`}, []string{`"b"`, `2`, `{"x": [2]}`, `{"x": [1], "y": 1}`}},
		{"const", `{"const": 10}`, []string{`10`, `1e1`, `10.00`}, []string{`11`, `"10"`}},
		{"minimum", `{"minimum": 1}`, []string{`1`, `2`, `"x"`}, []string{`0.999`}},
		{"maximum", `{"maximum": 1.5}`, []string{`1.5`}, []string{`1.5000001`}},
		{"exclusiveMinimum", `{"exclusiveMinimum": 0}`, []string{`0.0001`}, []string{`0`, `-1`}},
		{"exclusiveMaximum", `{"exclusiveMaximum": 10}`, []string{`9.99`}, []string{`10`}},
		{"exact rationals", `{"maximum": 0.1}`, []string{`0.1`, `0.10`}, []string{`0.10000000000000000001`}},
		{"big integers", `{"maximum": 9007199254740993}`, []string{`9007199254740993`}, []string{`9007199254740994`}},
		{"minLength code points", `{"minLength": 2, "maxLength": 2}`, []string{`"日本"`, `"ab"`, `1`}, []string{`"日"`, `"abc"`}},
		{"pattern unanchored", `{"pattern": "^[a-z]+$"}`, []string{`"abc"`}, []string{`"abc1"`}},
		{"pattern substring", `{"pattern": "b"}`, []string{`"abc"`}, []string{`"ac"`}},
		{"items", `{"items": {"type": "integer"}, "minItems": 1, "maxItems": 2}`, []string{`[1]`, `[1, 2]`}, []string{`[]`, `[1, 2, 3]`, `["a"]`}},
		{"required", `{"required": ["a"]}`, []string{`{"a": null}`, `[]`}, []string{`{}`, `{"b": 1}`}},
		{"properties", `{"properties": {"a": {"type": "string"}}}`, []string{`{}`, `{"a": "x", "b": 1}`}, []string{`{"a": 1}`}},
		{"additional false", `{"properties": {"a": {}}, "additionalProperties": false}`, []string{`{"a": 1}`}, []string{`{"b": 1}`}},
		{"additional schema", `{"additionalProperties": {"type": "integer"}}`, []string{`{"b": 1}`}, []string{`{"b": "x"}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustCompile(t, c.schema)
			for _, v := range c.ok {
				if err := s.Validate([]byte(v)); err != nil {
					t.Errorf("Validate(%s): %v", v, err)
				}
			}
			for _, v := range c.bad {
				var ve *ValidationError
				if err := s.Validate([]byte(v)); !errors.As(err, &ve) {
					t.Errorf("Validate(%s) = %v, want a *ValidationError", v, err)
				}
			}
		})
	}
}

func TestValidateListsEveryProblem(t *testing.T) {
	s := mustCompile(t, `{
		"type": "object",
		"required": ["name", "limit"],
		"additionalProperties": false,
		"properties": {
			"name": {"type": "string", "minLength": 1},
			"tags": {"type": "array", "items": {"type": "string"}},
			"a/b~c": {"type": "integer"}
		}
	}`)
	err := s.Validate([]byte(`{"name": "", "tags": ["x", 2, 3], "extra": 1, "a/b~c": "no"}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate = %v, want a *ValidationError", err)
	}
	want := []Problem{
		{"", `missing required property "limit"`},
		{"/a~1b~0c", "must be integer, not string"},
		{"/extra", "is not an allowed property"},
		{"/name", "must be at least 1 characters long"},
		{"/tags/1", "must be string, not number"},
		{"/tags/2", "must be string, not number"},
	}
	if !reflect.DeepEqual(ve.Problems, want) {
		t.Fatalf("problems:\n got %#v\nwant %#v", ve.Problems, want)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, `(root): missing required property "limit"; /a~1b~0c:`) {
		t.Fatalf("Error() = %q", msg)
	}
}

func TestValidateBoundsExponents(t *testing.T) {
	if _, err := Compile([]byte(`{"maximum": 1e1001}`)); err == nil {
		t.Fatal("Compile accepted a bound beyond the exponent limit")
	}
	s := mustCompile(t, `{"type": "integer", "maximum": 1e1000}`)
	if err := s.Validate([]byte(`1e1000`)); err != nil {
		t.Fatalf("Validate(1e1000): %v", err)
	}
	for _, v := range []string{`1e999999999`, `1E-5000`} {
		var ve *ValidationError
		if err := s.Validate([]byte(v)); !errors.As(err, &ve) {
			t.Errorf("Validate(%s) = %v, want a *ValidationError", v, err)
		}
	}
	// A number type check, enum and const need no exact value.
	if err := mustCompile(t, `{"type": "number"}`).Validate([]byte(`1e999999999`)); err == nil {
		t.Error("an out-of-range number passed with no problem")
	}
	if err := mustCompile(t, `{"enum": [1]}`).Validate([]byte(`1e999999999`)); err == nil {
		t.Error("an out-of-range number matched an enum")
	}
}

func TestValidateRefusesInvalidJSON(t *testing.T) {
	s := mustCompile(t, `{}`)
	for _, v := range []string{``, `{`, `1 2`, `1x`, `{}]`, `"a"}`, "\"\xff\""} {
		err := s.Validate([]byte(v))
		var ve *ValidationError
		if err == nil || errors.As(err, &ve) {
			t.Errorf("Validate(%q) = %v, want a decoding error", v, err)
		}
	}
}
