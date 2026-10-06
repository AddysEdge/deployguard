package compare

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/AddysEdge/deployguard/internal/verdict"
)

func mustJSON(t *testing.T, s string) Observed {
	t.Helper()
	o, err := Observe("GET", 200, "application/json", "", false, []byte(s), nil)
	if err != nil {
		t.Fatalf("Observe(%s): %v", s, err)
	}
	return o
}

func summarize(fs []Finding) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, fmt.Sprintf("%s %s %s %s->%s", f.Severity, f.Category, f.Path, f.Baseline, f.Candidate))
	}
	return strings.Join(parts, "; ")
}

func TestCompareJSON(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		cand   string
		opts   Options
		want   string // summarize() output
		result verdict.Outcome
	}{
		{"identical", `{"a":1,"b":[1,2]}`, `{"a":1,"b":[1,2]}`, Options{}, "", verdict.Pass},
		{"key order ignored", `{"a":1,"b":{"x":true,"y":null}}`, `{"b":{"y":null,"x":true},"a":1}`, Options{}, "", verdict.Pass},
		{"numeric equivalence", `{"n":1,"m":1.50,"z":0,"e":100}`, `{"n":1.0,"m":1.5,"z":-0.0,"e":1e2}`, Options{}, "", verdict.Pass},
		{"big integers exact", `{"id":9007199254740993}`, `{"id":9007199254740992}`, Options{}, "FAIL value_changed /id number->number", verdict.Fail},
		{"high precision decimals", `{"x":0.10000000000000000001}`, `{"x":0.1}`, Options{}, "FAIL value_changed /x number->number", verdict.Fail},
		{"number vs string", `{"total":129.5}`, `{"total":"129.5"}`, Options{}, "FAIL type_changed /total number->string", verdict.Fail},
		{"removed field", `{"id":1,"email":"a@b"}`, `{"id":1}`, Options{}, "FAIL field_removed /email string->missing", verdict.Fail},
		{"added field warns", `{"id":1}`, `{"id":1,"badge":"new"}`, Options{}, "WARN field_added /badge missing->string", verdict.Warn},
		{"added field strict fails", `{"id":1}`, `{"id":1,"badge":"new"}`, Options{StrictAdditions: true}, "FAIL field_added /badge missing->string", verdict.Fail},
		{"null vs missing", `{"manager":null}`, `{}`, Options{}, "FAIL field_removed /manager null->missing", verdict.Fail},
		{"missing vs null", `{}`, `{"manager":null}`, Options{}, "WARN field_added /manager missing->null", verdict.Warn},
		{"null to value is type change", `{"manager":null}`, `{"manager":"bob"}`, Options{}, "FAIL type_changed /manager null->string", verdict.Fail},
		{"nested value", `{"a":{"b":{"c":"x"}}}`, `{"a":{"b":{"c":"y"}}}`, Options{}, "FAIL value_changed /a/b/c string->string", verdict.Fail},
		{"bool change", `{"ok":true}`, `{"ok":false}`, Options{}, "FAIL value_changed /ok boolean->boolean", verdict.Fail},
		{"array order matters", `{"r":["a","b"]}`, `{"r":["b","a"]}`, Options{}, "FAIL value_changed /r/0 string->string; FAIL value_changed /r/1 string->string", verdict.Fail},
		{"array shorter", `[1,2,3]`, `[1,2]`, Options{}, "FAIL array_length_changed  length 3->length 2", verdict.Fail},
		{"array element field", `{"items":[{"sku":"a","qty":1}]}`, `{"items":[{"sku":"a","qty":"1"}]}`, Options{}, "FAIL type_changed /items/0/qty number->string", verdict.Fail},
		{"root type change", `{}`, `[]`, Options{}, "FAIL type_changed  object->array", verdict.Fail},
		{"escaped keys", `{"a/b":1,"m~n":1}`, `{"a/b":2,"m~n":2}`, Options{}, "FAIL value_changed /a~1b number->number; FAIL value_changed /m~0n number->number", verdict.Fail},
		{"empty key", `{"":1}`, `{"":2}`, Options{}, "FAIL value_changed / number->number", verdict.Fail},
		{"sorted output", `{"z":1,"a":1,"m":{"q":1}}`, `{"z":2,"a":"1","n":1}`, Options{}, "FAIL type_changed /a number->string; FAIL field_removed /m object->missing; WARN field_added /n missing->number; FAIL value_changed /z number->number", verdict.Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compare(mustJSON(t, tt.base), mustJSON(t, tt.cand), tt.opts)
			if got := summarize(res.Findings); got != tt.want {
				t.Fatalf("findings:\n got: %s\nwant: %s", got, tt.want)
			}
			if got := res.Outcome(); got != tt.result {
				t.Fatalf("outcome = %s, want %s", got, tt.result)
			}
		})
	}
}

func TestFindingsNeverContainScalarValues(t *testing.T) {
	res := Compare(mustJSON(t, `{"email":"alice@example.com","pin":1234}`), mustJSON(t, `{"email":"mallory@example.com","pin":9876}`), Options{})
	s := fmt.Sprintf("%+v", res.Findings)
	for _, secret := range []string{"alice", "mallory", "1234", "9876"} {
		if strings.Contains(s, secret) {
			t.Fatalf("finding exposes value %q: %s", secret, s)
		}
	}
}

func TestIgnoreRules(t *testing.T) {
	tests := []struct {
		name       string
		base, cand string
		ignore     []string
		want       string
		suppressed map[string]int
	}{
		{"top-level scalar", `{"requestId":"a","x":1}`, `{"requestId":"b","x":1}`, []string{"/requestId"}, "", map[string]int{"/requestId": 1}},
		{"nested scalar", `{"meta":{"ts":"1"}}`, `{"meta":{"ts":"2"}}`, []string{"/meta/ts"}, "", map[string]int{"/meta/ts": 1}},
		{"escaped token", `{"a/b":{"m~n":1}}`, `{"a/b":{"m~n":2}}`, []string{"/a~1b/m~0n"}, "", map[string]int{"/a~1b/m~0n": 1}},
		{"array index", `{"items":[{"ts":1},{"ts":2}]}`, `{"items":[{"ts":9},{"ts":3}]}`, []string{"/items/1/ts"}, "FAIL value_changed /items/0/ts number->number", map[string]int{"/items/1/ts": 1}},
		{"does not hide removal", `{"ts":"1"}`, `{}`, []string{"/ts"}, "FAIL field_removed /ts string->missing", map[string]int{}},
		{"does not hide addition", `{}`, `{"ts":"1"}`, []string{"/ts"}, "WARN field_added /ts missing->string", map[string]int{}},
		{"does not hide type change", `{"ts":"1"}`, `{"ts":1}`, []string{"/ts"}, "FAIL type_changed /ts string->number", map[string]int{}},
		{"does not hide null change", `{"ts":null}`, `{"ts":"x"}`, []string{"/ts"}, "FAIL type_changed /ts null->string", map[string]int{}},
		{"does not hide subtree", `{"meta":{"a":1}}`, `{"meta":{"a":2}}`, []string{"/meta"}, "FAIL value_changed /meta/a number->number", map[string]int{}},
		{"does not hide array length", `{"tags":[1]}`, `{"tags":[1,2]}`, []string{"/tags"}, "FAIL array_length_changed /tags length 1->length 2", map[string]int{}},
		{"exact path only", `{"a":{"ts":1},"b":{"ts":1}}`, `{"a":{"ts":2},"b":{"ts":2}}`, []string{"/a/ts"}, "FAIL value_changed /b/ts number->number", map[string]int{"/a/ts": 1}},
		{"missing path is unused", `{"x":1}`, `{"x":1}`, []string{"/nope"}, "", map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compare(mustJSON(t, tt.base), mustJSON(t, tt.cand), Options{Ignore: tt.ignore})
			if got := summarize(res.Findings); got != tt.want {
				t.Fatalf("findings:\n got: %s\nwant: %s", got, tt.want)
			}
			if fmt.Sprint(res.Suppressed) != fmt.Sprint(tt.suppressed) {
				t.Fatalf("suppressed = %v, want %v", res.Suppressed, tt.suppressed)
			}
		})
	}
}

func TestParseJSONRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"syntax":         `{"a":}`,
		"truncated":      `{"a":1`,
		"trailing value": `{"a":1} {"b":2}`,
		"trailing text":  `{"a":1} x`,
		"duplicate key":  `{"a":1,"a":2}`,
		"huge exponent":  `{"a":1e9999999999}`,
		"too deep":       strings.Repeat("[", 600) + strings.Repeat("]", 600),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(doc)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestParseJSONErrorsDoNotEchoContent(t *testing.T) {
	_, err := ParseJSON([]byte(`{"password":"hunter2",}`))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error should be content-free: %v", err)
	}
}

func TestCanonicalNumber(t *testing.T) {
	cases := map[string]string{
		"0": "0", "-0": "0", "0.000": "0", "0e10": "0",
		"1": "1e0", "1.0": "1e0", "10": "1e1", "1e1": "1e1", "100e-2": "1e0",
		"-12.340": "-1234e-2", "0.001": "1e-3", "1.5E+3": "15e2",
	}
	for in, want := range cases {
		got, err := canonicalNumber(in)
		if err != nil || got != want {
			t.Errorf("canonicalNumber(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestLookup(t *testing.T) {
	v, err := ParseJSON([]byte(`{"a":{"b/c":[10,{"~d":null}]},"":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Kind{"/a": Object, "/a/b~1c": Array, "/a/b~1c/0": Number, "/a/b~1c/1/~0d": Null, "/": Object, "//x": Number}
	for p, k := range found {
		got, ok := Lookup(v, p)
		if !ok || got.Kind != k {
			t.Errorf("Lookup(%q) = %v,%v want kind %s", p, got, ok, k)
		}
	}
	for _, p := range []string{"/missing", "/a/b~1c/2", "/a/b~1c/01", "/a/b~1c/-", "/a/b~1c/0/deeper", "a"} {
		if _, ok := Lookup(v, p); ok {
			t.Errorf("Lookup(%q) should not resolve", p)
		}
	}
}

func TestMediaTypes(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"application/json":                    "application/json",
		"Application/JSON; charset=UTF-8":     "application/json",
		"application/json;charset=utf-8":      "application/json",
		"text/csv; header=present; charset=x": "text/csv",
		"application/problem+json":            "application/problem+json",
		"garbage;;":                           "garbage",
	} {
		if got := NormalizeMediaType(in); got != want {
			t.Errorf("NormalizeMediaType(%q) = %q want %q", in, got, want)
		}
	}
	if !IsJSON("application/json") || !IsJSON("application/vnd.api+json") || IsJSON("text/json-ish") || IsJSON("text/plain") {
		t.Fatal("IsJSON misclassifies")
	}
}

func TestObserveMalformedDeclaredJSON(t *testing.T) {
	if _, err := Observe("GET", 200, "application/json", "", false, []byte(`{"a":`), nil); err == nil {
		t.Fatal("malformed declared JSON must be an error")
	}
	// Empty JSON bodies (e.g. 204) and HEAD are not parsed.
	if o, err := Observe("GET", 204, "application/json", "", false, nil, nil); err != nil || o.JSON != nil {
		t.Fatalf("empty body: %v", err)
	}
	if _, err := Observe("HEAD", 200, "application/json", "", false, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Non-JSON media types are compared as bytes, never parsed.
	if o, err := Observe("GET", 200, "text/plain", "", false, []byte(`{"a":`), nil); err != nil || o.JSON != nil {
		t.Fatal("text/plain should not be parsed")
	}
}

func TestCompareResponseLevel(t *testing.T) {
	baseURL, _ := url.Parse("http://127.0.0.1:8080/api/redirect")
	candURL, _ := url.Parse("http://127.0.0.1:9090/api/redirect")
	obs := func(u *url.URL, method string, status int, ct, loc string, hasLoc bool, body string) Observed {
		o, err := Observe(method, status, ct, loc, hasLoc, []byte(body), u)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	tests := []struct {
		name string
		b, c Observed
		want string
	}{
		{"status change", obs(baseURL, "GET", 200, "application/json", "", false, `{}`), obs(candURL, "GET", 404, "application/json", "", false, `{}`), "FAIL status_changed  200->404"},
		{"charset formatting ignored", obs(baseURL, "GET", 200, "text/csv; charset=utf-8", "", false, "a"), obs(candURL, "GET", 200, "Text/CSV;charset=UTF-8", "", false, "a"), ""},
		{"media type change", obs(baseURL, "GET", 200, "application/json", "", false, `{}`), obs(candURL, "GET", 200, "text/html", "", false, `<p>`), "FAIL media_type_changed  application/json->text/html"},
		{"missing content type", obs(baseURL, "GET", 200, "text/plain", "", false, "a"), obs(candURL, "GET", 200, "", "", false, "a"), "FAIL media_type_changed  text/plain->(none)"},
		{"non-JSON bytes", obs(baseURL, "GET", 200, "text/plain", "", false, "secret-a"), obs(candURL, "GET", 200, "text/plain", "", false, "secret-bb"), "FAIL body_changed  8 bytes sha256:"},
		{"head ignores body", obs(baseURL, "HEAD", 200, "text/csv", "", false, ""), obs(candURL, "HEAD", 200, "text/csv", "", false, ""), ""},
		{"same-origin absolute redirects match across hosts", obs(baseURL, "GET", 302, "", "http://127.0.0.1:8080/api/users/42", true, ""), obs(candURL, "GET", 302, "", "http://127.0.0.1:9090/api/users/42", true, ""), ""},
		{"relative vs absolute same-origin", obs(baseURL, "GET", 302, "", "/api/users/42", true, ""), obs(candURL, "GET", 302, "", "http://127.0.0.1:9090/api/users/42", true, ""), ""},
		{"default port normalized", obs(mustURL(t, "http://a.example/x"), "GET", 302, "", "http://a.example:80/y", true, ""), obs(mustURL(t, "http://b.example/x"), "GET", 302, "", "/y", true, ""), ""},
		{"redirect path change", obs(baseURL, "GET", 302, "", "/api/users/42?token=abc", true, ""), obs(candURL, "GET", 302, "", "/login?next=1", true, ""), "FAIL location_changed  /api/users/42?<redacted>->/login?<redacted>"},
		{"cross-origin redirect differs", obs(baseURL, "GET", 302, "", "/api/users/42", true, ""), obs(candURL, "GET", 302, "", "https://evil.example/api/users/42", true, ""), "FAIL location_changed  /api/users/42->cross-origin https://evil.example/api/users/42"},
		{"location removed", obs(baseURL, "GET", 302, "", "/a", true, ""), obs(candURL, "GET", 200, "", "", false, ""), "FAIL status_changed  302->200; FAIL location_changed  /a->(no Location)"},
		{"empty vs JSON body", obs(baseURL, "GET", 200, "application/json", "", false, ""), obs(candURL, "GET", 200, "application/json", "", false, `{}`), "FAIL body_changed  empty->2 bytes sha256:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarize(Compare(tt.b, tt.c, Options{}).Findings)
			if !strings.HasPrefix(got, tt.want) || (tt.want == "" && got != "") {
				t.Fatalf("got %q want prefix %q", got, tt.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "token=abc") {
				t.Fatalf("finding leaks content: %s", got)
			}
		})
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// object renders a JSON object from ordered key/value fragments.
func object(pairs ...string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:%s", pairs[i], pairs[i+1])
	}
	b.WriteString("}")
	return b.String()
}

// keys returns n key/value pairs prefix000..prefix(n-1) with the given value.
func keys(prefix string, n int, value string) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s%03d", prefix, i), value)
	}
	return out
}

// TestFindingCapNeverChangesVerdict guards the release-gate invariant: the
// cap limits retained findings, never the outcome or the counts.
func TestFindingCapNeverChangesVerdict(t *testing.T) {
	tests := []struct {
		name         string
		base, cand   string
		want         verdict.Outcome
		counts       Counts
		retainedFail int
	}{
		{
			// Keys sort a000..a099 then z: 100 WARN additions are discovered
			// before the FAIL removal of /z.
			name:         "100 added-field WARNs then a removed-field FAIL",
			base:         object("z", "1"),
			cand:         object(keys("a", 100, "1")...),
			want:         verdict.Fail,
			counts:       Counts{Total: 101, Fail: 1, Warn: 100, Retained: 100, Omitted: 1, OmittedWarn: 1},
			retainedFail: 1,
		},
		{
			name:   "more than 100 WARNs and no FAIL",
			base:   object("id", "1"),
			cand:   object(append([]string{"id", "1"}, keys("b", 150, "1")...)...),
			want:   verdict.Warn,
			counts: Counts{Total: 150, Warn: 150, Retained: 100, Omitted: 50, OmittedWarn: 50},
		},
		{
			// /a type change (FAIL) before the cap, 100 additions, /z removal (FAIL) after it.
			name:         "FAILs before and after the cap",
			base:         object("a", "1", "z", "1"),
			cand:         object(append([]string{"a", `"1"`}, keys("b", 100, "1")...)...),
			want:         verdict.Fail,
			counts:       Counts{Total: 102, Fail: 2, Warn: 100, Retained: 100, Omitted: 2, OmittedWarn: 2},
			retainedFail: 2,
		},
		{
			name:         "more FAILs than the cap",
			base:         object(keys("k", 150, "1")...),
			cand:         object(keys("k", 150, "2")...),
			want:         verdict.Fail,
			counts:       Counts{Total: 150, Fail: 150, Retained: 100, Omitted: 50, OmittedFail: 50},
			retainedFail: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Compare(mustJSON(t, tt.base), mustJSON(t, tt.cand), Options{})
			if got := res.Outcome(); got != tt.want {
				t.Fatalf("outcome = %s, want %s (counts %+v)", got, tt.want, res.Counts)
			}
			if res.Counts != tt.counts {
				t.Fatalf("counts = %+v, want %+v", res.Counts, tt.counts)
			}
			if len(res.Findings) != tt.counts.Retained || res.Omitted != tt.counts.Omitted {
				t.Fatalf("retained %d omitted %d disagree with counts %+v", len(res.Findings), res.Omitted, res.Counts)
			}
			fails := 0
			for _, f := range res.Findings {
				if f.Severity == verdict.Fail {
					fails++
				}
			}
			if fails != tt.retainedFail {
				t.Fatalf("retained %d FAIL findings, want %d (FAILs are retained in preference to WARNs)", fails, tt.retainedFail)
			}
		})
	}
}

func TestRetainedFindingsAreDeterministic(t *testing.T) {
	base := object("a", "1", "z", "1")
	cand := object(append([]string{"a", `"1"`}, keys("b", 120, "1")...)...)
	first := Compare(mustJSON(t, base), mustJSON(t, cand), Options{})
	for i := 0; i < 5; i++ {
		again := Compare(mustJSON(t, base), mustJSON(t, cand), Options{})
		if summarize(again.Findings) != summarize(first.Findings) || again.Counts != first.Counts {
			t.Fatal("retained findings differ between identical comparisons")
		}
	}
	for i := 1; i < len(first.Findings); i++ {
		if first.Findings[i-1].Path > first.Findings[i].Path {
			t.Fatalf("retained findings not sorted by path at %d: %s > %s", i, first.Findings[i-1].Path, first.Findings[i].Path)
		}
	}
	if first.Findings[0].Path != "/a" || first.Findings[len(first.Findings)-1].Path != "/z" {
		t.Fatalf("both FAIL findings should be retained: first %s last %s", first.Findings[0].Path, first.Findings[len(first.Findings)-1].Path)
	}
}
