package compare

import (
	"strconv"
	"strings"
)

var (
	pointerEscaper   = strings.NewReplacer("~", "~0", "/", "~1")
	pointerUnescaper = strings.NewReplacer("~1", "/", "~0", "~")
)

// escapeToken encodes an object key as an RFC 6901 reference token.
func escapeToken(key string) string { return pointerEscaper.Replace(key) }

// Lookup resolves an RFC 6901 JSON Pointer against v. Array tokens must be
// canonical decimal indexes ("0", "12"; not "01" or "-").
func Lookup(v *Value, pointer string) (*Value, bool) {
	if pointer == "" {
		return v, v != nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	cur := v
	for _, raw := range strings.Split(pointer[1:], "/") {
		if cur == nil {
			return nil, false
		}
		tok := pointerUnescaper.Replace(raw)
		switch cur.Kind {
		case Object:
			next, ok := cur.Object[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case Array:
			if tok == "" || (len(tok) > 1 && tok[0] == '0') {
				return nil, false
			}
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(cur.Array) {
				return nil, false
			}
			cur = cur.Array[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
