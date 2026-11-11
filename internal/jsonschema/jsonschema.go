// Package jsonschema validates JSON values against a documented subset of
// JSON Schema (ADR 0011, item 6). Compile refuses a schema that uses a
// keyword outside the subset, so nothing a schema says goes unenforced.
//
// Supported: type (one name or a list of names), properties, required,
// additionalProperties (a boolean or a schema), items (one schema), enum,
// const, minimum, maximum, exclusiveMinimum, exclusiveMaximum (numbers),
// minLength, maxLength, pattern (Go's RE2 syntax, unanchored), minItems,
// maxItems and boolean schemas. The annotations $schema, $comment, title,
// description, default, examples and deprecated are accepted and validate
// nothing. Numbers compare exactly, as rationals; a string's length counts
// code points.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is a compiled schema. It is safe for concurrent use.
type Schema struct {
	root *node
}

// node is one compiled schema. A boolean schema sets only boolean.
type node struct {
	boolean    *bool
	properties map[string]*node
	additional *node
	items      *node
	pattern    *regexp.Regexp
	min, max   *big.Rat
	exMin      *big.Rat
	exMax      *big.Rat
	minLen     *int
	maxLen     *int
	minItems   *int
	maxItems   *int
	constV     any
	types      []string
	required   []string
	enum       []any
	hasConst   bool
}

// isAnnotation reports whether k is an annotation the subset accepts and
// that validates nothing.
func isAnnotation(k string) bool {
	switch k {
	case "$schema", "$comment", "title", "description", "default", "examples", "deprecated":
		return true
	}
	return false
}

const typeNumber = "number"

func isTypeName(t string) bool {
	switch t {
	case "object", "array", "string", typeNumber, "integer", "boolean", "null":
		return true
	}
	return false
}

// Compile compiles a schema. It returns an error for a schema that is not
// valid JSON, is neither an object nor a boolean, uses a keyword outside
// the subset, or gives a keyword an invalid value.
func Compile(raw json.RawMessage) (*Schema, error) {
	v, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("jsonschema: %w", err)
	}
	n, err := compile(v, "")
	if err != nil {
		return nil, err
	}
	return &Schema{root: n}, nil
}

func decode(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("value is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("value is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("value is not valid JSON: data after the value")
	}
	return v, nil
}

func compile(v any, at string) (*node, error) {
	switch s := v.(type) {
	case bool:
		return &node{boolean: &s}, nil
	case map[string]any:
		n := &node{}
		for _, k := range sortedKeys(s) {
			if err := n.keyword(k, s[k], at); err != nil {
				return nil, err
			}
		}
		return n, nil
	}
	return nil, fmt.Errorf("jsonschema: schema at %s is neither an object nor a boolean", display(at))
}

func (n *node) keyword(k string, v any, at string) error {
	var err error
	switch k {
	case "type":
		if n.types, err = typeList(v); err != nil {
			return badValue(k, at, "a type name or a list of distinct type names")
		}
	case "properties":
		return n.compileProperties(v, at)
	case "required":
		if n.required, err = stringSet(v); err != nil {
			return badValue(k, at, "a list of distinct strings")
		}
	case "additionalProperties":
		n.additional, err = compile(v, at+"/additionalProperties")
		return err
	case "items":
		n.items, err = compile(v, at+"/items")
		return err
	default:
		return n.valueKeyword(k, v, at)
	}
	return nil
}

// valueKeyword compiles a keyword that constrains a value rather than its
// structure, or an annotation.
func (n *node) valueKeyword(k string, v any, at string) error {
	switch k {
	case "enum":
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return badValue(k, at, "a non-empty list")
		}
		n.enum = list
	case "const":
		n.constV, n.hasConst = v, true
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
		return n.compileBound(k, v, at)
	case "minLength", "maxLength", "minItems", "maxItems":
		return n.compileCount(k, v, at)
	case "pattern":
		pat, ok := v.(string)
		if !ok {
			return badValue(k, at, "a string")
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return badValue(k, at, "a valid RE2 regular expression")
		}
		n.pattern = re
	default:
		if !isAnnotation(k) {
			return fmt.Errorf("jsonschema: keyword %q at %s is not supported (ADR 0011, item 6)", k, display(at))
		}
	}
	return nil
}

func badValue(k, at, want string) error {
	return fmt.Errorf("jsonschema: %q at %s must be %s", k, display(at), want)
}

func (n *node) compileProperties(v any, at string) error {
	m, ok := v.(map[string]any)
	if !ok {
		return badValue("properties", at, "an object of schemas")
	}
	n.properties = make(map[string]*node, len(m))
	for _, p := range sortedKeys(m) {
		var err error
		if n.properties[p], err = compile(m[p], at+"/properties/"+escape(p)); err != nil {
			return err
		}
	}
	return nil
}

func (n *node) compileBound(k string, v any, at string) error {
	r, ok := rat(v)
	if !ok {
		return badValue(k, at, "a number")
	}
	switch k {
	case "minimum":
		n.min = r
	case "maximum":
		n.max = r
	case "exclusiveMinimum":
		n.exMin = r
	default:
		n.exMax = r
	}
	return nil
}

func (n *node) compileCount(k string, v any, at string) error {
	c, ok := count(v)
	if !ok {
		return badValue(k, at, "a non-negative integer")
	}
	switch k {
	case "minLength":
		n.minLen = &c
	case "maxLength":
		n.maxLen = &c
	case "minItems":
		n.minItems = &c
	default:
		n.maxItems = &c
	}
	return nil
}

func typeList(v any) ([]string, error) {
	if s, ok := v.(string); ok {
		v = []any{s}
	}
	names, err := stringSet(v)
	if err != nil || len(names) == 0 {
		return nil, errors.New("invalid")
	}
	for _, t := range names {
		if !isTypeName(t) {
			return nil, errors.New("invalid")
		}
	}
	return names, nil
}

func stringSet(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("invalid")
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok || slices.Contains(out, s) {
			return nil, errors.New("invalid")
		}
		out = append(out, s)
	}
	return out, nil
}

// maxExponent bounds a number's decimal exponent: big.Rat computes a
// number's exact value, so 1e999999999 would take unbounded memory.
const maxExponent = 1000

func rat(v any) (*big.Rat, bool) {
	num, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	s := string(num)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil || exp > maxExponent || exp < -maxExponent {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}

func count(v any) (int, bool) {
	r, ok := rat(v)
	if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() || r.Num().Int64() > int64(^uint32(0)>>1) {
		return 0, false
	}
	return int(r.Num().Int64()), true
}

// Problem is one way a value fails its schema: where, as a JSON Pointer
// (the empty string is the whole value), and what.
type Problem struct {
	Pointer string
	Message string
}

// ValidationError lists every problem Validate found, in a stable order.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = display(p.Pointer) + ": " + p.Message
	}
	return strings.Join(parts, "; ")
}

// Validate validates a JSON value. A value that is not valid JSON returns
// an error; one the schema rejects returns a *ValidationError listing
// every problem.
func (s *Schema) Validate(raw json.RawMessage) error {
	v, err := decode(raw)
	if err != nil {
		return err
	}
	var probs []Problem
	s.root.validate(v, "", &probs)
	if len(probs) > 0 {
		return &ValidationError{Problems: probs}
	}
	return nil
}

func (n *node) validate(v any, at string, probs *[]Problem) {
	add := func(format string, args ...any) {
		*probs = append(*probs, Problem{Pointer: at, Message: fmt.Sprintf(format, args...)})
	}
	if n.boolean != nil {
		if !*n.boolean {
			add("no value is allowed here")
		}
		return
	}
	if len(n.types) > 0 && !slices.ContainsFunc(n.types, func(t string) bool { return hasType(v, t) }) {
		add("must be %s, not %s", strings.Join(n.types, " or "), typeOf(v))
		return
	}
	if n.hasConst && !equal(v, n.constV) {
		add("must be %s", compact(n.constV))
	}
	if n.enum != nil && !slices.ContainsFunc(n.enum, func(e any) bool { return equal(v, e) }) {
		add("must be one of %s", compact(n.enum))
	}
	switch x := v.(type) {
	case json.Number:
		n.validateNumber(x, add)
	case string:
		n.validateString(x, add)
	case []any:
		n.validateArray(x, at, add, probs)
	case map[string]any:
		n.validateObject(x, at, add, probs)
	}
}

func (n *node) validateNumber(x json.Number, add func(string, ...any)) {
	r, ok := rat(x)
	if !ok {
		add("is outside the supported range (exponent beyond ±%d)", maxExponent)
		return
	}
	switch {
	case n.min != nil && r.Cmp(n.min) < 0:
		add("must be at least %s", n.min.RatString())
	case n.exMin != nil && r.Cmp(n.exMin) <= 0:
		add("must be greater than %s", n.exMin.RatString())
	}
	switch {
	case n.max != nil && r.Cmp(n.max) > 0:
		add("must be at most %s", n.max.RatString())
	case n.exMax != nil && r.Cmp(n.exMax) >= 0:
		add("must be less than %s", n.exMax.RatString())
	}
}

func (n *node) validateString(x string, add func(string, ...any)) {
	l := utf8.RuneCountInString(x)
	if n.minLen != nil && l < *n.minLen {
		add("must be at least %d characters long", *n.minLen)
	}
	if n.maxLen != nil && l > *n.maxLen {
		add("must be at most %d characters long", *n.maxLen)
	}
	if n.pattern != nil && !n.pattern.MatchString(x) {
		add("must match the pattern %q", n.pattern.String())
	}
}

func (n *node) validateArray(x []any, at string, add func(string, ...any), probs *[]Problem) {
	if n.minItems != nil && len(x) < *n.minItems {
		add("must have at least %d items", *n.minItems)
	}
	if n.maxItems != nil && len(x) > *n.maxItems {
		add("must have at most %d items", *n.maxItems)
	}
	if n.items != nil {
		for i, e := range x {
			n.items.validate(e, fmt.Sprintf("%s/%d", at, i), probs)
		}
	}
}

func (n *node) validateObject(x map[string]any, at string, add func(string, ...any), probs *[]Problem) {
	for _, r := range n.required {
		if _, ok := x[r]; !ok {
			add("missing required property %q", r)
		}
	}
	for _, k := range sortedKeys(x) {
		child := at + "/" + escape(k)
		if p, ok := n.properties[k]; ok {
			p.validate(x[k], child, probs)
		} else if n.additional != nil {
			if n.additional.boolean != nil && !*n.additional.boolean {
				*probs = append(*probs, Problem{Pointer: child, Message: "is not an allowed property"})
				continue
			}
			n.additional.validate(x[k], child, probs)
		}
	}
}

func hasType(v any, t string) bool {
	switch t {
	case "integer":
		num, ok := v.(json.Number)
		if !ok {
			return false
		}
		r, ok := rat(num)
		return ok && r.IsInt()
	case typeNumber:
		_, ok := v.(json.Number)
		return ok
	}
	return typeOf(v) == t
}

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return typeNumber
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

// equal is JSON equality: numbers compare by value.
func equal(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		ra, okA := rat(x)
		rb, okB := rat(y)
		if !okA || !okB {
			return x == y
		}
		return ra.Cmp(rb) == 0
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, equal)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !equal(xv, yv) {
				return false
			}
		}
		return true
	}
	return a == b
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// escape escapes a JSON Pointer reference token (RFC 6901).
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func display(pointer string) string {
	if pointer == "" {
		return "(root)"
	}
	return pointer
}
