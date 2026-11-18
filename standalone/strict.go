package standalone

import "github.com/SnapdragonPartners/maestro-agents/internal/strictjson"

// decodeStrict decodes an operator's file strictly (ADR 0017, item 5): no
// invalid UTF-8, no key that is not exactly a field's JSON name, no key
// repeated in any object, no data after the value.
func decodeStrict(data []byte, v any) error {
	return strictjson.Decode(data, v) //nolint:wrapcheck // the caller names the file
}
