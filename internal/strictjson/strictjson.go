// Package strictjson decodes JSON more strictly than encoding/json, for
// files an operator writes: the standalone host's configuration and
// secrets (ADR 0017, item 5) and the intake agent's scoring mapping
// (ADR 0020, item 7); and for the intake agent's extraction records,
// from a model or a person (ADR 0021, item 13).
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decode decodes data into v, which must be a pointer, more strictly
// than encoding/json: it refuses invalid UTF-8, a key
// that is not exactly a field's JSON name (encoding/json matches keys
// case-insensitively), a key repeated in any object (encoding/json keeps
// the last), and data after the value.
func Decode(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := walk(dec, reflect.TypeOf(v).Elem(), ""); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err //nolint:wrapcheck // the caller names the file
	}
	return nil
}

var (
	rawMessageType  = reflect.TypeFor[json.RawMessage]()  //nolint:gochecknoglobals // a constant type, never written
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]() //nolint:gochecknoglobals // a constant type, never written
)

// walk reads one JSON value from dec, checking it against t. A nil t, an
// interface, json.RawMessage or a type with its own UnmarshalJSON is
// checked for repeated keys only.
func walk(dec *json.Decoder, t reflect.Type, at string) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && (t == rawMessageType || t.Kind() == reflect.Interface || reflect.PointerTo(t).Implements(unmarshalerType)) {
		t = nil
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", display(at), err)
	}
	switch tok {
	case json.Delim('{'):
		return walkObject(dec, t, at)
	case json.Delim('['):
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := 0; dec.More(); i++ {
			if err := walk(dec, elem, at+"/"+strconv.Itoa(i)); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err            //nolint:wrapcheck // the decoder's syntax error is the message
	}
	return nil
}

func walkObject(dec *json.Decoder, t reflect.Type, at string) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", display(at), err)
		}
		key, _ := tok.(string)
		path := at + "/" + escapePointer(key)
		if seen[key] {
			return fmt.Errorf("%s: key %q is repeated", display(at), key)
		}
		seen[key] = true
		var next reflect.Type
		switch {
		case t == nil:
		case t.Kind() == reflect.Struct:
			f, ok := fieldByJSONName(t, key)
			if !ok {
				return fmt.Errorf("%s: unknown field %q", display(at), key)
			}
			next = f
		case t.Kind() == reflect.Map:
			next = t.Elem()
		}
		if err := walk(dec, next, path); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err            //nolint:wrapcheck // the decoder's syntax error is the message
}

// fieldByJSONName returns the type of t's field whose JSON name is exactly
// name, following encoding/json's rules for embedded structs: an untagged
// embedded struct's fields are promoted (by JSON name, not Go name), the
// shallowest wins, a tagged one beats an untagged one at the same depth,
// and names still tied are no field at all.
func fieldByJSONName(t reflect.Type, name string) (reflect.Type, bool) {
	level := []reflect.Type{t}
	seen := map[reflect.Type]bool{}
	for len(level) > 0 {
		found, next := fieldsNamed(level, name, seen)
		if len(found) > 0 {
			return dominant(found)
		}
		level = next
	}
	return nil, false
}

// candidate is a field whose JSON name matched, and whether it is tagged.
type candidate struct {
	t      reflect.Type
	tagged bool
}

// fieldsNamed returns the fields of one depth's structs whose JSON name is
// name, and the untagged embedded structs whose fields are the next depth.
func fieldsNamed(level []reflect.Type, name string, seen map[reflect.Type]bool) (found []candidate, next []reflect.Type) {
	for _, st := range level {
		if seen[st] {
			continue
		}
		seen[st] = true
		for i := range st.NumField() {
			f := st.Field(i)
			n, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if n == "-" {
				continue
			}
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			embedded := f.Anonymous && ft.Kind() == reflect.Struct
			switch {
			case embedded && n == "":
				next = append(next, ft) // promoted, even if unexported
			case !f.IsExported() && !embedded:
				// encoding/json keeps a tagged, unexported embedded struct
			case n == name:
				found = append(found, candidate{f.Type, true})
			case n == "" && f.Name == name:
				found = append(found, candidate{f.Type, false})
			}
		}
	}
	return found, next
}

// dominant picks encoding/json's field among one depth's matches: the one
// tagged field, else the one untagged field; a tie is no field.
func dominant(found []candidate) (reflect.Type, bool) {
	var tagged []candidate
	for _, c := range found {
		if c.tagged {
			tagged = append(tagged, c)
		}
	}
	if len(tagged) == 0 {
		tagged = found
	}
	if len(tagged) != 1 {
		return nil, false
	}
	return tagged[0].t, true
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func display(at string) string {
	if at == "" {
		return "(root)"
	}
	return at
}
