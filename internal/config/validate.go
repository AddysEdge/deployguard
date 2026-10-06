package config

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AddysEdge/deployguard/internal/verdict"
)

var scenarioName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type problems []string

func (p *problems) addf(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func validate(raw *rawFile, lookup LookupFunc) (*Config, error) {
	var errs problems
	cfg := &Config{}

	switch {
	case raw.Version == nil:
		errs.addf("version: required (use version: %d)", SupportedVersion)
	case *raw.Version != SupportedVersion:
		errs.addf("version: unsupported version %d (this build supports %d)", *raw.Version, SupportedVersion)
	default:
		cfg.Version = *raw.Version
	}

	cfg.Settings = validateSettings(raw.Settings, &errs)

	switch {
	case len(raw.Scenarios) == 0:
		errs.addf("scenarios: at least one scenario is required")
	case len(raw.Scenarios) > MaxScenarios:
		errs.addf("scenarios: %d scenarios exceeds the maximum of %d", len(raw.Scenarios), MaxScenarios)
	}

	seen := map[string]int{}
	for i, rs := range raw.Scenarios {
		label := fmt.Sprintf("scenarios[%d]", i)
		if rs.Name != "" {
			label = fmt.Sprintf("scenarios[%d] (%s)", i, rs.Name)
		}
		sc := validateScenario(rs, cfg.Settings, label, lookup, &errs)
		if rs.Name != "" {
			if j, dup := seen[rs.Name]; dup {
				errs.addf("%s.name: duplicate scenario name (also used by scenarios[%d])", label, j)
			} else {
				seen[rs.Name] = i
			}
		}
		cfg.Scenarios = append(cfg.Scenarios, sc)
	}

	if len(errs) == 0 {
		if planned := cfg.PlannedRequests(); planned > cfg.Settings.MaxTotalRequests {
			errs.addf("settings.max_total_requests: workload plans %d requests, which exceeds the limit of %d", planned, cfg.Settings.MaxTotalRequests)
		}
	}
	if len(errs) > 0 {
		return nil, &ValidationError{Problems: errs}
	}
	return cfg, nil
}

func validateSettings(rs *rawSettings, errs *problems) Settings {
	s := Settings{
		Timeout:          DefaultTimeout,
		MaxResponseBytes: DefaultMaxResponseBytes,
		Concurrency:      DefaultConcurrency,
		MaxTotalRequests: DefaultMaxTotalRequests,
	}
	if rs == nil {
		return s
	}
	if rs.Timeout != nil {
		s.Timeout = time.Duration(*rs.Timeout)
		checkTimeout("settings.timeout", s.Timeout, errs)
	}
	if rs.MaxResponseBytes != nil {
		s.MaxResponseBytes = *rs.MaxResponseBytes
		if s.MaxResponseBytes <= 0 || s.MaxResponseBytes > MaxMaxResponseBytes {
			errs.addf("settings.max_response_bytes: must be between 1 and %d", MaxMaxResponseBytes)
		}
	}
	if rs.Concurrency != nil {
		s.Concurrency = *rs.Concurrency
		checkRange("settings.concurrency", s.Concurrency, 1, MaxConcurrency, errs)
	}
	if rs.MaxTotalRequests != nil {
		s.MaxTotalRequests = *rs.MaxTotalRequests
		checkRange("settings.max_total_requests", s.MaxTotalRequests, 1, MaxMaxTotalRequests, errs)
	}
	return s
}

func checkTimeout(field string, d time.Duration, errs *problems) {
	if d <= 0 || d > MaxTimeout {
		errs.addf("%s: must be greater than 0 and at most %s", field, MaxTimeout)
	}
}

func checkRange(field string, v, lo, hi int, errs *problems) {
	if v < lo || v > hi {
		errs.addf("%s: must be between %d and %d (got %d)", field, lo, hi, v)
	}
}

func validateScenario(rs rawScenario, settings Settings, label string, lookup LookupFunc, errs *problems) Scenario {
	sc := Scenario{
		Name:            rs.Name,
		Path:            rs.Path,
		Timeout:         settings.Timeout,
		StrictAdditions: rs.StrictAdditions != nil && *rs.StrictAdditions,
	}

	if rs.Name == "" {
		errs.addf("%s.name: required", label)
	} else if !scenarioName.MatchString(rs.Name) {
		errs.addf("%s.name: must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit", label)
	}

	method := strings.ToUpper(strings.TrimSpace(rs.Method))
	switch method {
	case http.MethodGet, http.MethodHead:
		sc.Method = method
	case "":
		errs.addf("%s.method: required (GET or HEAD)", label)
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		errs.addf("%s.method: %s is not supported in v1; DeployGuard only replays GET and HEAD", label, method)
	default:
		errs.addf("%s.method: unsupported method %q (GET or HEAD)", label, rs.Method)
	}

	if err := validatePath(rs.Path); err != nil {
		errs.addf("%s.path: %v", label, err)
	}

	sc.Query = url.Values{}
	for k, v := range rs.Query {
		if k == "" {
			errs.addf("%s.query: parameter names must not be empty", label)
			continue
		}
		sc.Query.Set(k, v)
	}

	sc.Headers = validateHeaders(rs.Headers, label, lookup, errs)

	if rs.Timeout != nil {
		sc.Timeout = time.Duration(*rs.Timeout)
		checkTimeout(label+".timeout", sc.Timeout, errs)
	}

	if rs.ExpectStatus != nil {
		sc.ExpectStatus = *rs.ExpectStatus
		checkRange(label+".expect_status", sc.ExpectStatus, 100, 599, errs)
	}

	seenPtr := map[string]bool{}
	for j, p := range rs.Ignore {
		field := fmt.Sprintf("%s.ignore[%d]", label, j)
		if err := ValidatePointer(p); err != nil {
			errs.addf("%s: %v", field, err)
			continue
		}
		if seenPtr[p] {
			errs.addf("%s: duplicate ignore path %q", field, p)
			continue
		}
		seenPtr[p] = true
		sc.Ignore = append(sc.Ignore, p)
	}
	if sc.Method == http.MethodHead {
		if len(rs.Ignore) > 0 {
			errs.addf("%s.ignore: HEAD responses have no body, so ignore rules cannot apply", label)
		}
		if rs.StrictAdditions != nil {
			errs.addf("%s.strict_additions: HEAD responses have no body, so this setting cannot apply", label)
		}
	}

	if rs.Performance != nil {
		sc.Performance = validatePerformance(rs.Performance, settings, label+".performance", errs)
	}
	return sc
}

func validatePerformance(rp *rawPerformance, settings Settings, label string, errs *problems) *Performance {
	p := &Performance{
		Warmup:      DefaultWarmup,
		Samples:     MinSamples,
		Concurrency: settings.Concurrency,
		Gate:        verdict.Warn,
	}
	if rp.Warmup != nil {
		p.Warmup = *rp.Warmup
		checkRange(label+".warmup", p.Warmup, 0, MaxWarmup, errs)
	}
	if rp.Samples != nil {
		p.Samples = *rp.Samples
		checkRange(label+".samples", p.Samples, MinSamples, MaxSamples, errs)
	}
	if rp.Concurrency != nil {
		p.Concurrency = *rp.Concurrency
		checkRange(label+".concurrency", p.Concurrency, 1, MaxConcurrency, errs)
	}
	if rp.P95RelativePct == nil {
		errs.addf("%s.p95_relative_pct: required (percentage the candidate p95 must exceed the baseline p95 by)", label)
	} else {
		p.P95RelativePct = *rp.P95RelativePct
		if !(p.P95RelativePct > 0 && p.P95RelativePct <= MaxRelativePct) {
			errs.addf("%s.p95_relative_pct: must be greater than 0 and at most %g", label, MaxRelativePct)
		}
	}
	if rp.P95AbsoluteMs == nil {
		errs.addf("%s.p95_absolute_ms: required (milliseconds the candidate p95 must exceed the baseline p95 by)", label)
	} else {
		p.P95AbsoluteMs = *rp.P95AbsoluteMs
		if !(p.P95AbsoluteMs > 0 && p.P95AbsoluteMs <= MaxAbsoluteMs) {
			errs.addf("%s.p95_absolute_ms: must be greater than 0 and at most %g", label, MaxAbsoluteMs)
		}
	}
	if rp.Gate != nil {
		switch strings.ToLower(*rp.Gate) {
		case "warn":
			p.Gate = verdict.Warn
		case "fail":
			p.Gate = verdict.Fail
		default:
			errs.addf("%s.gate: must be \"warn\" or \"fail\" (got %q)", label, *rp.Gate)
		}
	}
	return p
}

// validatePath accepts only root-relative paths that cannot change the
// configured origin. Query strings belong in the query field.
func validatePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("required")
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("must be root-relative and start with \"/\"")
	case strings.HasPrefix(p, "//"):
		return fmt.Errorf("must not start with \"//\" (that would be a scheme-relative URL naming another host)")
	case strings.ContainsAny(p, "?#"):
		return fmt.Errorf("must not contain '?' or '#'; use the query field for parameters")
	}
	for _, r := range p {
		if r <= ' ' || r >= 0x7f || r == '\\' {
			return fmt.Errorf("must not contain whitespace, control characters, backslashes or non-ASCII characters (percent-encode them)")
		}
	}
	u, err := url.Parse(p)
	if err != nil {
		return fmt.Errorf("malformed path (check percent-encoding)")
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("must be root-relative and must not name a scheme or host")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("must not contain \".\" or \"..\" segments")
		}
	}
	return nil
}

// ValidatePointer checks that p is a non-root RFC 6901 JSON Pointer.
func ValidatePointer(p string) error {
	if strings.HasPrefix(p, "$") {
		return fmt.Errorf("%q looks like JSONPath; use an RFC 6901 JSON Pointer such as \"/requestId\" or \"/metadata/timestamp\"", p)
	}
	if p == "" {
		return fmt.Errorf("the root pointer \"\" would ignore the whole document and is not allowed")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q is not a JSON Pointer; it must start with \"/\"", p)
	}
	if strings.ContainsAny(p, "*") {
		return fmt.Errorf("%q: wildcards are not supported in v1", p)
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '~' && (i+1 >= len(p) || (p[i+1] != '0' && p[i+1] != '1')) {
			return fmt.Errorf("%q: '~' must be escaped as ~0, and '/' inside a key as ~1", p)
		}
	}
	return nil
}

// OriginKey normalizes an http(s) URL's origin for equality checks: lowercased
// scheme and hostname plus the effective port (80/443 when omitted). It does
// no DNS resolution, so different hostnames are always different origins.
func OriginKey(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// EquivalentOrigins reports whether two origins are the same after
// normalization, e.g. http://example.test and http://EXAMPLE.test:80.
func EquivalentOrigins(a, b *url.URL) bool { return OriginKey(a) == OriginKey(b) }

// ParseOrigin validates a baseline or candidate URL. Only http(s) origins are
// accepted: no userinfo, query, fragment or path other than "/". The error
// never echoes the input, which could contain credentials.
func ParseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("not a valid URL (expected an origin such as http://127.0.0.1:8080)")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme != "http" && scheme != "https":
		return nil, fmt.Errorf("scheme must be http or https")
	case u.User != nil:
		return nil, fmt.Errorf("must not contain userinfo (credentials in URLs are not allowed; use a header with ${ENV_VAR})")
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("host is required")
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("must be an origin without a query string")
	case u.Fragment != "":
		return nil, fmt.Errorf("must be an origin without a fragment")
	case u.Path != "" && u.Path != "/":
		return nil, fmt.Errorf("must be an origin without a path (scenario paths are root-relative)")
	}
	if port := u.Port(); port != "" {
		var n int
		if _, err := fmt.Sscanf(port, "%d", &n); err != nil || n < 1 || n > 65535 || fmt.Sprint(n) != port {
			return nil, fmt.Errorf("invalid port")
		}
	}
	return &url.URL{Scheme: scheme, Host: strings.ToLower(u.Host)}, nil
}
