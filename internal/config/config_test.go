package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AddysEdge/deployguard/internal/verdict"
)

func noEnv(string) (string, bool) { return "", false }

func envMap(m map[string]string) LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const minimal = `
version: 1
scenarios:
  - name: users
    method: GET
    path: /api/users/42
`

func TestParseMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Settings
	if s.Timeout != DefaultTimeout || s.MaxResponseBytes != DefaultMaxResponseBytes || s.Concurrency != DefaultConcurrency || s.MaxTotalRequests != DefaultMaxTotalRequests {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	sc := cfg.Scenarios[0]
	if sc.Method != "GET" || sc.Timeout != DefaultTimeout || sc.Performance != nil || sc.StrictAdditions || sc.ExpectStatus != 0 {
		t.Fatalf("unexpected scenario defaults: %+v", sc)
	}
	if got := cfg.PlannedRequests(); got != 4 {
		t.Fatalf("PlannedRequests = %d, want 4", got)
	}
}

func TestParseFullScenario(t *testing.T) {
	doc := `
version: 1
settings:
  timeout: 2s
  max_response_bytes: 2048
  concurrency: 8
  max_total_requests: 1000
scenarios:
  - name: search
    method: get
    path: /api/search
    query: {q: pen, page: 2}
    headers:
      Accept: application/json
      authorization: "Bearer ${DG_TOKEN}"
    timeout: 750ms
    expect_status: 200
    ignore: ["/requestId", "/meta/a~1b"]
    strict_additions: true
    performance:
      warmup: 5
      samples: 120
      concurrency: 2
      p95_relative_pct: 50
      p95_absolute_ms: 25.5
      gate: fail
`
	cfg, err := Parse([]byte(doc), envMap(map[string]string{"DG_TOKEN": "s3cr3t-value"}))
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Scenarios[0]
	if sc.Method != "GET" || sc.Timeout != 750*time.Millisecond || sc.ExpectStatus != 200 || !sc.StrictAdditions {
		t.Fatalf("scenario: %+v", sc)
	}
	if sc.Query.Get("page") != "2" || sc.Query.Get("q") != "pen" {
		t.Fatalf("query: %v", sc.Query)
	}
	if len(sc.Headers) != 2 || sc.Headers[1].Name != "Authorization" || sc.Headers[1].Value() != "Bearer s3cr3t-value" || !sc.Headers[1].Sensitive() {
		t.Fatalf("headers: %#v", sc.Headers)
	}
	p := sc.Performance
	if p.Warmup != 5 || p.Samples != 120 || p.Concurrency != 2 || p.P95RelativePct != 50 || p.P95AbsoluteMs != 25.5 || p.Gate != verdict.Fail {
		t.Fatalf("performance: %+v", p)
	}
	if got, want := cfg.PlannedRequests(), 4+2*2*(5+120); got != want {
		t.Fatalf("PlannedRequests = %d, want %d", got, want)
	}
}

func TestPerformanceDefaults(t *testing.T) {
	doc := minimal + `    performance: {p95_relative_pct: 20, p95_absolute_ms: 10}
`
	cfg, err := Parse([]byte(doc), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Scenarios[0].Performance
	if p.Warmup != DefaultWarmup || p.Samples != MinSamples || p.Concurrency != DefaultConcurrency || p.Gate != verdict.Warn {
		t.Fatalf("performance defaults: %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	scenario := func(extra string) string {
		return "version: 1\nscenarios:\n  - name: s\n    method: GET\n    path: /x\n" + extra
	}
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"empty", "", "empty"},
		{"missing version", "scenarios: [{name: s, method: GET, path: /x}]", "version: required"},
		{"unknown version", "version: 2\nscenarios: [{name: s, method: GET, path: /x}]", "unsupported version 2"},
		{"unknown top-level field", minimal + "extra: true\n", "field extra not found"},
		{"unknown scenario field", scenario("    body: '{}'\n"), "field body not found"},
		{"unknown performance field", scenario("    performance: {p99: 1, p95_relative_pct: 1, p95_absolute_ms: 1}\n"), "field p99 not found"},
		{"duplicate key", scenario("    path: /y\n"), "already defined"},
		{"extra document", minimal + "---\nversion: 1\n", "exactly one YAML document"},
		{"no scenarios", "version: 1\nscenarios: []\n", "at least one scenario"},
		{"wrong type", "version: one\nscenarios: [{name: s, method: GET, path: /x}]", "cannot unmarshal"},
		{"integer duration", "version: 1\nsettings: {timeout: 5}\nscenarios: [{name: s, method: GET, path: /x}]", "duration must be a string"},
		{"bad duration", "version: 1\nsettings: {timeout: soon}\nscenarios: [{name: s, method: GET, path: /x}]", "invalid duration"},
		{"zero timeout", "version: 1\nsettings: {timeout: 0s}\nscenarios: [{name: s, method: GET, path: /x}]", "settings.timeout"},
		{"negative scenario timeout", scenario("    timeout: -1s\n"), "(s).timeout"},
		{"timeout too large", "version: 1\nsettings: {timeout: 2m}\nscenarios: [{name: s, method: GET, path: /x}]", "at most 1m0s"},
		{"zero concurrency", "version: 1\nsettings: {concurrency: 0}\nscenarios: [{name: s, method: GET, path: /x}]", "settings.concurrency"},
		{"concurrency above hard max", "version: 1\nsettings: {concurrency: 17}\nscenarios: [{name: s, method: GET, path: /x}]", "between 1 and 16"},
		{"response bytes zero", "version: 1\nsettings: {max_response_bytes: 0}\nscenarios: [{name: s, method: GET, path: /x}]", "max_response_bytes"},
		{"response bytes too big", "version: 1\nsettings: {max_response_bytes: 999999999}\nscenarios: [{name: s, method: GET, path: /x}]", "max_response_bytes"},
		{"post rejected", "version: 1\nscenarios: [{name: s, method: POST, path: /x}]", "POST is not supported in v1"},
		{"delete rejected", "version: 1\nscenarios: [{name: s, method: DELETE, path: /x}]", "DELETE is not supported in v1"},
		{"odd method", "version: 1\nscenarios: [{name: s, method: TRACE, path: /x}]", "unsupported method"},
		{"missing name", "version: 1\nscenarios: [{method: GET, path: /x}]", "name: required"},
		{"bad name", "version: 1\nscenarios: [{name: 'a b', method: GET, path: /x}]", ".name: must be"},
		{"duplicate names", "version: 1\nscenarios: [{name: s, method: GET, path: /x}, {name: s, method: GET, path: /y}]", "duplicate scenario name"},
		{"relative path", "version: 1\nscenarios: [{name: s, method: GET, path: x}]", "start with \"/\""},
		{"absolute url path", "version: 1\nscenarios: [{name: s, method: GET, path: 'http://evil.example/x'}]", "start with \"/\""},
		{"scheme-relative path", "version: 1\nscenarios: [{name: s, method: GET, path: //evil.example/x}]", "must not start with \"//\""},
		{"query in path", "version: 1\nscenarios: [{name: s, method: GET, path: '/x?a=1'}]", "use the query field"},
		{"fragment in path", "version: 1\nscenarios: [{name: s, method: GET, path: '/x#f'}]", "'?' or '#'"},
		{"dot segments", "version: 1\nscenarios: [{name: s, method: GET, path: /a/../b}]", "\"..\" segments"},
		{"encoded dot segments", "version: 1\nscenarios: [{name: s, method: GET, path: /a/%2e%2e/b}]", "\"..\" segments"},
		{"bad percent encoding", "version: 1\nscenarios: [{name: s, method: GET, path: /a%zz}]", "malformed path"},
		{"space in path", "version: 1\nscenarios: [{name: s, method: GET, path: '/a b'}]", "whitespace"},
		{"backslash in path", "version: 1\nscenarios: [{name: s, method: GET, path: '/a\\b'}]", "backslashes"},
		{"expect status out of range", scenario("    expect_status: 99\n"), "expect_status"},
		{"jsonpath ignore", scenario("    ignore: ['$.requestId']\n"), "looks like JSONPath"},
		{"non pointer ignore", scenario("    ignore: ['requestId']\n"), "must start with \"/\""},
		{"root ignore", scenario("    ignore: ['']\n"), "root pointer"},
		{"wildcard ignore", scenario("    ignore: ['/items/*/id']\n"), "wildcards"},
		{"bad escape ignore", scenario("    ignore: ['/a~2b']\n"), "~0"},
		{"duplicate ignore", scenario("    ignore: ['/a', '/a']\n"), "duplicate ignore path"},
		{"head with ignore", "version: 1\nscenarios: [{name: s, method: HEAD, path: /x, ignore: [/a]}]", "HEAD responses have no body"},
		{"perf missing thresholds", scenario("    performance: {}\n"), "p95_relative_pct: required"},
		{"perf missing absolute", scenario("    performance: {p95_relative_pct: 10}\n"), "p95_absolute_ms: required"},
		{"perf too few samples", scenario("    performance: {samples: 99, p95_relative_pct: 10, p95_absolute_ms: 5}\n"), "samples: must be between 100"},
		{"perf zero relative", scenario("    performance: {p95_relative_pct: 0, p95_absolute_ms: 5}\n"), "p95_relative_pct: must be greater than 0"},
		{"perf negative absolute", scenario("    performance: {p95_relative_pct: 5, p95_absolute_ms: -1}\n"), "p95_absolute_ms: must be greater than 0"},
		{"perf bad gate", scenario("    performance: {p95_relative_pct: 5, p95_absolute_ms: 5, gate: block}\n"), "gate: must be"},
		{"perf concurrency too high", scenario("    performance: {concurrency: 32, p95_relative_pct: 5, p95_absolute_ms: 5}\n"), "concurrency: must be between 1 and 16"},
		{"planned requests cap", "version: 1\nsettings: {max_total_requests: 100}\n" + strings.TrimPrefix(scenario("    performance: {p95_relative_pct: 5, p95_absolute_ms: 5}\n"), "version: 1\n"), "plans 444 requests"},
		{"forbidden header", scenario("    headers: {Host: evil.example}\n"), "Host is managed"},
		{"content-type header", scenario("    headers: {content-type: application/json}\n"), "Content-Type is managed"},
		{"duplicate header case", scenario("    headers: {Accept: a, accept: b}\n"), "case-insensitive"},
		{"invalid header name", scenario("    headers: {'Bad Header': x}\n"), "invalid header name"},
		{"missing env var", scenario("    headers: {Authorization: 'Bearer ${DG_MISSING}'}\n"), "DG_MISSING is not set"},
		{"invalid env ref", scenario("    headers: {X-Thing: '${1BAD}'}\n"), "invalid environment reference"},
		{"unterminated env ref", scenario("    headers: {X-Thing: 'abc${NAME'}\n"), "unterminated"},
		{"header CRLF", scenario("    headers: {X-Thing: \"a\\r\\nInjected: yes\"}\n"), "control characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc), noEnv)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
			if strings.Contains(err.Error(), "config.raw") {
				t.Fatalf("error leaks Go type names: %q", err.Error())
			}
		})
	}
}

func TestValidationCollectsAllProblems(t *testing.T) {
	doc := "version: 1\nsettings: {concurrency: 0}\nscenarios: [{name: s, method: POST, path: x}]"
	_, err := Parse([]byte(doc), noEnv)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	if len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %d: %v", len(ve.Problems), ve.Problems)
	}
}

func TestSecretsNeverEchoed(t *testing.T) {
	const secret = "tok_ABC123_super_secret"
	env := envMap(map[string]string{"DG_TOKEN": secret, "DG_BAD": "line1\r\nline2-" + secret})

	// A valid config must not expose the secret through formatting.
	ok := minimal + "    headers: {Authorization: 'Bearer ${DG_TOKEN}', X-Plain: 'v'}\n"
	cfg, err := Parse([]byte(ok), env)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(form, cfg.Scenarios[0]); strings.Contains(s, secret) {
			t.Fatalf("format %s leaked secret: %s", form, s)
		}
		if s := fmt.Sprintf(form, cfg); strings.Contains(s, secret) {
			t.Fatalf("format %s leaked secret via *Config", form)
		}
	}
	if !cfg.Scenarios[0].Headers[0].Sensitive() {
		t.Fatal("interpolated header should be sensitive")
	}
	if cfg.Scenarios[0].Headers[1].Sensitive() {
		t.Fatal("plain X-Plain header should not be sensitive")
	}

	// An invalid resolved value must be rejected without echoing it.
	bad := minimal + "    headers: {X-Thing: '${DG_BAD}'}\n"
	_, err = Parse([]byte(bad), env)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "line1") {
		t.Fatalf("expected redacted error, got %v", err)
	}
}

func TestIsSensitiveHeader(t *testing.T) {
	for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-API-KEY", "Apikey", "X-Auth-Token", "X-Client-Secret", "X-Session-Id"} {
		if !IsSensitiveHeader(h) {
			t.Errorf("%s should be sensitive", h)
		}
	}
	for _, h := range []string{"Accept", "User-Agent", "X-Request-Source"} {
		if IsSensitiveHeader(h) {
			t.Errorf("%s should not be sensitive", h)
		}
	}
}

func TestParseOrigin(t *testing.T) {
	good := map[string]string{
		"http://127.0.0.1:8080":   "http://127.0.0.1:8080",
		"http://127.0.0.1:8080/":  "http://127.0.0.1:8080",
		"HTTPS://Staging.Example": "https://staging.example",
		"http://[::1]:9000":       "http://[::1]:9000",
	}
	for in, want := range good {
		u, err := ParseOrigin(in)
		if err != nil {
			t.Errorf("ParseOrigin(%q): %v", in, err)
			continue
		}
		if u.String() != want {
			t.Errorf("ParseOrigin(%q) = %s, want %s", in, u, want)
		}
	}
	bad := map[string]string{
		"":                              "scheme",
		"127.0.0.1:8080":                "URL",
		"ftp://example.com":             "scheme",
		"http://user:hunter2@host":      "userinfo",
		"http://host?x=1":               "query",
		"http://host/?":                 "query",
		"http://host#frag":              "fragment",
		"http://host/api":               "path",
		"http://":                       "host",
		"http://host:0":                 "port",
		"http://host:99999":             "port",
		"http://host:abc":               "URL",
		"javascript:alert(1)":           "scheme",
		"http://host:8080/../../etc":    "path",
		"https://user@internal.example": "userinfo",
	}
	for in, want := range bad {
		_, err := ParseOrigin(in)
		if err == nil {
			t.Errorf("ParseOrigin(%q) succeeded, want error containing %q", in, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseOrigin(%q) error %q, want it to contain %q", in, err, want)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("ParseOrigin leaked credentials: %v", err)
		}
	}
}

func TestLoadFileAndExamples(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "w.yaml")
	if err := os.WriteFile(p, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, noEnv); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), noEnv); err == nil {
		t.Fatal("expected error for missing file")
	}

	// Every shipped example must validate.
	matches, _ := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	more, _ := filepath.Glob(filepath.Join("..", "..", "examples", "eval", "*.yaml"))
	for _, m := range append(matches, more...) {
		if _, err := Load(m, noEnv); err != nil {
			t.Errorf("example %s: %v", m, err)
		}
	}
}

func TestEquivalentOrigins(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{"http://example.test", "http://example.test", true},
		{"http://example.test", "http://EXAMPLE.test:80", true},
		{"https://example.test/", "https://example.test:443", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/", true},
		{"http://[::1]:9000", "http://[::1]:9000", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8081", false}, // distinct ports
		{"http://example.test", "https://example.test", false},    // scheme
		{"http://example.test:443", "https://example.test", false},
		{"http://localhost:8080", "http://127.0.0.1:8080", false}, // no DNS resolution
		{"http://a.example.test", "http://b.example.test", false},
	}
	for _, tt := range tests {
		a, err := ParseOrigin(tt.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseOrigin(tt.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := EquivalentOrigins(a, b); got != tt.same {
			t.Errorf("EquivalentOrigins(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.same)
		}
	}
}
