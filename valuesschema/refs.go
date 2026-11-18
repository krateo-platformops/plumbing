package valuesschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// pointerEscape escapes one JSON-pointer reference token (RFC 6901: "~" -> "~0", "/" -> "~1").
// Helm dependency names and aliases are [a-zA-Z0-9_-], so this is a no-op for every key a
// dependency can have; it is here so the pointer is correct by construction, not by luck.
func pointerEscape(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// rebaseLocalRefs returns schema with every LOCAL "$ref" (one that starts with "#/", or is exactly
// "#") moved under base, the JSON pointer the schema is embedded at: "#/$defs/port" becomes
// base+"/$defs/port", and "#" becomes base. Anything else is left exactly as written: a plain-name
// anchor ref ("#name") is document-global by definition, and a ref to another document is not this
// package's to resolve. Key order and every other byte of meaning are preserved; the output is
// compact.
//
// Only the value of a "$ref" key is rewritten, so a string that merely looks like a pointer (a
// "default", an "enum" member, a "description") is never touched.
func rebaseLocalRefs(schema []byte, base string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var out bytes.Buffer
	if err := copyValue(dec, &out, base, false); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the schema")
	}
	return out.Bytes(), nil
}

// copyValue copies one JSON value from dec to out. isRef is whether this value is the value of a
// "$ref" key.
func copyValue(dec *json.Decoder, out *bytes.Buffer, base string, isRef bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out.WriteByte('{')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					out.WriteByte(',')
				}
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return fmt.Errorf("object key %v is not a string", kt)
				}
				if err := writeJSON(out, key); err != nil {
					return err
				}
				out.WriteByte(':')
				if err := copyValue(dec, out, base, key == "$ref"); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // '}'
				return err
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					out.WriteByte(',')
				}
				if err := copyValue(dec, out, base, false); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // ']'
				return err
			}
			out.WriteByte(']')
		default:
			return fmt.Errorf("unexpected delimiter %v", t)
		}
		return nil
	case string:
		if isRef {
			switch {
			case t == "#":
				t = base
			case strings.HasPrefix(t, "#/"):
				t = base + t[1:]
			}
		}
		return writeJSON(out, t)
	default: // json.Number, bool, nil
		return writeJSON(out, t)
	}
}

func writeJSON(out *bytes.Buffer, v any) error {
	b, err := marshalValue(v)
	if err != nil {
		return err
	}
	out.Write(b)
	return nil
}
