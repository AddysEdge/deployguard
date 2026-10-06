// Package compare implements DeployGuard's semantic response comparison: a
// precise JSON tree (no float conversion, duplicate keys rejected), a
// recursive diff with RFC 6901 paths, exact-path value ignores, and status,
// media type, redirect Location and opaque-body comparison.
package compare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Kind is a JSON value type.
type Kind int

const (
	Null Kind = iota
	Bool
	Number
	String
	Object
	Array
)

func (k Kind) String() string {
	switch k {
	case Null:
		return "null"
	case Bool:
		return "boolean"
	case Number:
		return "number"
	case String:
		return "string"
	case Object:
		return "object"
	case Array:
		return "array"
	}
	return "unknown"
}

// Value is a parsed JSON value. Numbers are stored in a canonical exact
// decimal form so that 1, 1.0 and 10e-1 compare equal without float rounding.
type Value struct {
	Kind   Kind
	Bool   bool
	Str    string // String value, or canonical Number
	Object map[string]*Value
	Array  []*Value
}

func (v *Value) scalarEqual(o *Value) bool {
	switch v.Kind {
	case Bool:
		return v.Bool == o.Bool
	case Number, String:
		return v.Str == o.Str
	case Null:
		return true
	}
	return false
}

const maxDepth = 512

// ParseJSON parses exactly one JSON document. It rejects trailing data,
// duplicate object keys (which make a document ambiguous), nesting deeper
// than 512 levels, and numbers whose exponent is out of the supported range.
func ParseJSON(data []byte) (*Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON document")
	}
	return v, nil
}

func parseValue(dec *json.Decoder, depth int) (*Value, error) {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("unexpected end of JSON input")
		}
		return nil, cleanJSONError(err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= maxDepth {
			return nil, fmt.Errorf("JSON nesting exceeds %d levels", maxDepth)
		}
		switch t {
		case '{':
			obj := map[string]*Value{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, cleanJSONError(err)
				}
				key, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				if _, dup := obj[key]; dup {
					return nil, errors.New("duplicate object key")
				}
				child, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = child
			}
			if _, err := dec.Token(); err != nil {
				return nil, cleanJSONError(err)
			}
			return &Value{Kind: Object, Object: obj}, nil
		case '[':
			arr := []*Value{}
			for dec.More() {
				child, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, child)
			}
			if _, err := dec.Token(); err != nil {
				return nil, cleanJSONError(err)
			}
			return &Value{Kind: Array, Array: arr}, nil
		}
		return nil, fmt.Errorf("unexpected %q", rune(t))
	case string:
		return &Value{Kind: String, Str: t}, nil
	case json.Number:
		c, err := canonicalNumber(string(t))
		if err != nil {
			return nil, err
		}
		return &Value{Kind: Number, Str: c}, nil
	case bool:
		return &Value{Kind: Bool, Bool: t}, nil
	case nil:
		return &Value{Kind: Null}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token")
}

// cleanJSONError keeps syntax errors free of document content.
func cleanJSONError(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("invalid JSON syntax at byte offset %d", se.Offset)
	}
	return errors.New("invalid JSON")
}

const maxExponent = 1_000_000_000

// canonicalNumber rewrites a JSON number literal as [-]D e E where D has no
// leading or trailing zeros. Two literals are numerically equal exactly when
// their canonical forms are equal. Zero is always "0" (so -0 == 0).
func canonicalNumber(s string) (string, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, expPart := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, expPart = s[:i], s[i+1:]
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	var exp int64
	if expPart != "" {
		e, err := strconv.ParseInt(expPart, 10, 64)
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", errors.New("JSON number exponent out of supported range")
		}
		exp = e
	}
	exp -= int64(len(frac))
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits) - len(trimmed))
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + strconv.FormatInt(exp, 10), nil
}
