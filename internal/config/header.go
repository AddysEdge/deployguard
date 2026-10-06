package config

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Header is a configured request header. Its resolved value is unexported so
// that it cannot be serialized or printed by accident; String and GoString
// always redact it.
type Header struct {
	Name      string // canonical form
	value     string
	sensitive bool
}

// Value returns the resolved header value for sending on the wire.
func (h Header) Value() string { return h.value }

// Sensitive reports whether the header is secret, either by name or because
// its value was interpolated from the environment.
func (h Header) Sensitive() bool { return h.sensitive }

func (h Header) String() string   { return h.Name + ": <redacted>" }
func (h Header) GoString() string { return "config.Header{" + h.Name + ": <redacted>}" }

// NewHeader builds a header programmatically (used by tests and fixtures).
func NewHeader(name, value string) Header {
	name = http.CanonicalHeaderKey(name)
	return Header{Name: name, value: value, sensitive: IsSensitiveHeader(name)}
}

// forbiddenHeaders are controlled by the HTTP client, would alter the target,
// or imply a request body (v1 has none).
var forbiddenHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true,
	"Upgrade": true, "Te": true, "Trailer": true, "Keep-Alive": true, "Proxy-Connection": true,
	"Expect": true, "Content-Type": true, "Content-Encoding": true,
}

var sensitiveHeaderParts = []string{"authorization", "cookie", "api-key", "apikey", "token", "secret", "session", "password", "credential"}

// IsSensitiveHeader reports whether a header name conventionally carries a
// secret (Authorization, Cookie, Set-Cookie, X-Api-Key, *-Token, ...).
func IsSensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	for _, part := range sensitiveHeaderParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isTokenChar(name[i]) {
			return false
		}
	}
	return true
}

func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

var (
	envRef  = regexp.MustCompile(`\$\{([^}]*)\}`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// interpolate replaces ${NAME} references with environment values. Any other
// use of "${" is an error. A "$" not followed by "{" is literal.
func interpolate(tmpl string, lookup LookupFunc) (string, bool, error) {
	var firstErr error
	used := false
	out := envRef.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := m[2 : len(m)-1]
		if !envName.MatchString(name) {
			if firstErr == nil {
				firstErr = fmt.Errorf("invalid environment reference %q (use ${NAME} with letters, digits and underscores)", m)
			}
			return ""
		}
		used = true
		v, ok := lookup(name)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s is not set", name)
			}
			return ""
		}
		return v
	})
	if firstErr != nil {
		return "", false, firstErr
	}
	if strings.Contains(envRef.ReplaceAllString(tmpl, ""), "${") {
		return "", false, fmt.Errorf("unterminated \"${\" in header value")
	}
	return out, used, nil
}

func validateHeaders(raw map[string]string, label string, lookup LookupFunc, errs *problems) []Header {
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	sort.Strings(names)

	var out []Header
	seen := map[string]string{}
	for _, name := range names {
		field := fmt.Sprintf("%s.headers[%s]", label, name)
		if !validHeaderName(name) {
			errs.addf("%s: invalid header name", field)
			continue
		}
		canon := http.CanonicalHeaderKey(name)
		if prev, dup := seen[canon]; dup {
			errs.addf("%s: duplicate of header %q (header names are case-insensitive)", field, prev)
			continue
		}
		seen[canon] = name
		if forbiddenHeaders[canon] {
			errs.addf("%s: header %s is managed by DeployGuard or implies a request body and cannot be set", field, canon)
			continue
		}
		value, fromEnv, err := interpolate(raw[name], lookup)
		if err != nil {
			errs.addf("%s: %v", field, err)
			continue
		}
		if !validHeaderValue(value) {
			// Never echo the value: it may contain a resolved secret.
			errs.addf("%s: value contains control characters (CR, LF or NUL are not allowed)", field)
			continue
		}
		out = append(out, Header{Name: canon, value: value, sensitive: fromEnv || IsSensitiveHeader(canon)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
