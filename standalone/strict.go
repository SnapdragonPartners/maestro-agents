package standalone

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

// decodeStrict decodes data into v, which must be a pointer, more strictly
// than encoding/json (ADR 0017, item 5): it refuses invalid UTF-8, a key
// that is not exactly a field's JSON name (encoding/json matches keys
// case-insensitively), a key repeated in any object (encoding/json keeps
// the last), and data after the value.
func decodeStrict(data []byte, v any) error {
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
// name.
func fieldByJSONName(t reflect.Type, name string) (reflect.Type, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		n, _, _ := strings.Cut(tag, ",")
		if n == "-" {
			continue
		}
		if n == "" {
			n = f.Name
		}
		if n == name {
			return f.Type, true
		}
	}
	return nil, false
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
