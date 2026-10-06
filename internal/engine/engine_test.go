package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/fixture"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/report"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// --- Evaluate: pure verdict rules ---

var (
	bURL, _ = url.Parse("http://127.0.0.1:8080/x")
	cURL, _ = url.Parse("http://127.0.0.1:8081/x")
)

func js(status int, body string) replay.Response {
	return replay.Response{Status: status, ContentType: "application/json", Body: []byte(body), BodyBytes: int64(len(body)), Duration: time.Millisecond}
}

func failed(kind replay.FailureKind) replay.Response {
	return replay.Response{Failure: &replay.Failure{Kind: kind, Message: "x"}}
}

func scenario(mod ...func(*config.Scenario)) config.Scenario {
	sc := config.Scenario{Name: "s", Method: "GET", Path: "/x", Query: url.Values{}}
	for _, m := range mod {
		m(&sc)
	}
	return sc
}

func TestEvaluateRules(t *testing.T) {
	ok := js(200, `{"a":1}`)
	tests := []struct {
		name   string
		sc     config.Scenario
		obs    []replay.Response // b1, c1, b2, c2
		want   verdict.Outcome
		reason string
	}{
		{"identical", scenario(), []replay.Response{ok, ok, ok, ok}, verdict.Pass, "matched baseline"},
		{"baseline 5xx", scenario(), []replay.Response{js(503, `{}`), ok, js(503, `{}`), ok}, verdict.Inconclusive, "server error status 503"},
		{"same 5xx on both sides is not pass", scenario(), []replay.Response{js(500, `{}`), js(500, `{}`), js(500, `{}`), js(500, `{}`)}, verdict.Inconclusive, "not a trustworthy reference"},
		{"baseline malformed JSON", scenario(), []replay.Response{js(200, `{"a":`), ok, js(200, `{"a":`), ok}, verdict.Inconclusive, "not valid JSON"},
		{"baseline timeout", scenario(), []replay.Response{failed(replay.FailTimeout), ok, ok, ok}, verdict.Inconclusive, "timeout"},
		{"baseline oversized", scenario(), []replay.Response{failed(replay.FailBodyLimit), ok, ok, ok}, verdict.Inconclusive, "body_limit"},
		{"baseline refused", scenario(), []replay.Response{failed(replay.FailRefused), ok, failed(replay.FailRefused), ok}, verdict.Inconclusive, "connection_refused"},
		{"candidate 5xx", scenario(), []replay.Response{ok, js(500, `{}`), ok, js(500, `{}`)}, verdict.Fail, "server error status 500"},
		{"candidate malformed JSON", scenario(), []replay.Response{ok, js(200, `nope`), ok, js(200, `nope`)}, verdict.Fail, "not valid JSON"},
		{"candidate timeout once", scenario(), []replay.Response{ok, ok, ok, failed(replay.FailTimeout)}, verdict.Fail, "timeout"},
		{"candidate oversized", scenario(), []replay.Response{ok, failed(replay.FailBodyLimit), ok, ok}, verdict.Fail, "body_limit"},
		{"expect_status baseline violates", scenario(func(s *config.Scenario) { s.ExpectStatus = 201 }), []replay.Response{ok, ok, ok, ok}, verdict.Inconclusive, "does not match expect_status 201"},
		{"expect_status candidate violates", scenario(func(s *config.Scenario) { s.ExpectStatus = 200 }), []replay.Response{ok, js(202, `{"a":1}`), ok, js(202, `{"a":1}`)}, verdict.Fail, "does not match expect_status 200"},
		{"baseline unstable", scenario(), []replay.Response{js(200, `{"v":1}`), ok, js(200, `{"v":2}`), ok}, verdict.Inconclusive, "baseline unstable"},
		{"baseline unstable then ignored", scenario(func(s *config.Scenario) { s.Ignore = []string{"/v"} }), []replay.Response{js(200, `{"v":1}`), js(200, `{"v":3}`), js(200, `{"v":2}`), js(200, `{"v":4}`)}, verdict.Pass, "matched"},
		{"candidate unstable", scenario(), []replay.Response{ok, ok, ok, js(200, `{"a":2}`)}, verdict.Fail, "candidate unstable"},
		{"added field warns", scenario(), []replay.Response{ok, js(200, `{"a":1,"b":2}`), ok, js(200, `{"a":1,"b":2}`)}, verdict.Warn, "0 FAIL and 1 WARN"},
		{"added field strict fails", scenario(func(s *config.Scenario) { s.StrictAdditions = true }), []replay.Response{ok, js(200, `{"a":1,"b":2}`), ok, js(200, `{"a":1,"b":2}`)}, verdict.Fail, "1 FAIL and 0 WARN"},
		{"status change", scenario(), []replay.Response{ok, js(404, `{"a":1}`), ok, js(404, `{"a":1}`)}, verdict.Fail, "differs"},
		{"canceled", scenario(), []replay.Response{ok, failed(replay.FailCanceled), failed(replay.FailCanceled), failed(replay.FailCanceled)}, verdict.Inconclusive, "canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Evaluate(tt.sc, bURL, cURL, tt.obs)
			if b.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s; reasons %v; findings %+v", b.Outcome, tt.want, b.Reasons, b.Findings)
			}
			if !strings.Contains(strings.Join(b.Reasons, " | "), tt.reason) {
				t.Fatalf("reasons %v do not mention %q", b.Reasons, tt.reason)
			}
			if len(b.Observations) != 4 || b.Observations[0].Target != "baseline" || b.Observations[1].Target != "candidate" {
				t.Fatalf("observations not interleaved: %+v", b.Observations)
			}
		})
	}
}

func TestIgnoreRuleReporting(t *testing.T) {
	sc := scenario(func(s *config.Scenario) {
		s.Ignore = []string{"/ts", "/missing", "/meta", "/kind", "/same"}
	})
	b := js(200, `{"ts":"1","meta":{"x":1},"kind":"a","same":1}`)
	c := js(200, `{"ts":"2","meta":{"x":1},"kind":1,"same":1}`)
	beh := Evaluate(sc, bURL, cURL, []replay.Response{b, c, b, c})
	if beh.Outcome != verdict.Fail {
		t.Fatalf("type change at an ignored path must still fail: %s", beh.Outcome)
	}
	got := map[string]report.IgnoreRule{}
	for _, r := range beh.IgnoreRules {
		got[r.Path] = r
	}
	expect := map[string]string{
		"/ts":      "applied",
		"/missing": "path not present",
		"/meta":    "holds object",
		"/kind":    "structural or type change",
		"/same":    "did not differ",
	}
	for p, want := range expect {
		r := got[p]
		if want == "applied" {
			if r.Status != "applied" || r.Suppressed != 1 {
				t.Errorf("%s: %+v", p, r)
			}
			continue
		}
		if r.Status != "unused" || !strings.Contains(r.Note, want) {
			t.Errorf("%s: %+v, want unused with note containing %q", p, r, want)
		}
	}
}

// --- Run: integration against real local servers ---

func startFixture(t *testing.T, v fixture.Variant) *url.URL {
	t.Helper()
	srv := httptest.NewServer(fixture.NewHandler(fixture.Options{Variant: v, SlowDelay: 60 * time.Millisecond}))
	t.Cleanup(srv.Close)
	u, err := config.ParseOrigin(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustConfig(t *testing.T, doc string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(doc), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const fixtureWorkload = `
version: 1
settings: {concurrency: 4, timeout: 2s}
scenarios:
  - {name: users, method: GET, path: /api/users/42}
  - {name: orders, method: GET, path: /api/orders/7}
  - {name: items, method: GET, path: /api/items/3, expect_status: 200}
  - {name: inventory, method: GET, path: /api/inventory}
  - {name: status, method: GET, path: /api/status, ignore: [/requestId, /timestamp]}
  - {name: profile, method: GET, path: /api/profile}
  - {name: catalog, method: GET, path: /api/catalog}
  - {name: export, method: HEAD, path: /api/export.csv}
  - {name: export-body, method: GET, path: /api/export.csv}
  - {name: redirect, method: GET, path: /api/redirect}
  - name: search
    method: GET
    path: /api/search
    query: {q: pen}
    performance: {warmup: 2, samples: 100, concurrency: 8, p95_relative_pct: 50, p95_absolute_ms: 25, gate: fail}
`

func outcomes(rep *report.Report) map[string]verdict.Outcome {
	m := map[string]verdict.Outcome{}
	for _, s := range rep.Scenarios {
		m[s.Name] = s.Outcome
	}
	return m
}

func TestRunCleanCandidatePasses(t *testing.T) {
	cfg := mustConfig(t, fixtureWorkload)
	rep := Run(context.Background(), cfg, startFixture(t, fixture.Baseline), startFixture(t, fixture.Clean), Options{Version: "test"})
	if rep.Outcome != verdict.Pass || rep.ExitCode != 0 {
		t.Fatalf("clean candidate: %s %v\n%v", rep.Outcome, rep.Reasons, outcomes(rep))
	}
	if rep.Run.IssuedRequests != int64(cfg.PlannedRequests()) {
		t.Fatalf("issued %d requests, planned %d", rep.Run.IssuedRequests, cfg.PlannedRequests())
	}
	for _, s := range rep.Scenarios {
		if s.Name == "search" {
			if s.Performance.Outcome != verdict.Pass || len(s.Performance.Rounds) != 2 {
				t.Fatalf("search performance: %+v", s.Performance)
			}
		} else if s.Performance.Outcome != verdict.Skipped {
			t.Fatalf("%s performance should be SKIPPED", s.Name)
		}
	}
}

func TestRunRegressedCandidate(t *testing.T) {
	cfg := mustConfig(t, fixtureWorkload)
	rep := Run(context.Background(), cfg, startFixture(t, fixture.Baseline), startFixture(t, fixture.Regressed), Options{Version: "test"})
	want := map[string]verdict.Outcome{
		"users": verdict.Fail, "orders": verdict.Fail, "items": verdict.Fail, "inventory": verdict.Fail,
		"status": verdict.Pass, "profile": verdict.Pass, "catalog": verdict.Warn,
		"export": verdict.Pass, "export-body": verdict.Pass, "redirect": verdict.Pass, "search": verdict.Fail,
	}
	got := outcomes(rep)
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %s, want %s", name, got[name], w)
		}
	}
	if rep.Outcome != verdict.Fail || rep.ExitCode != 1 || rep.Summary.Fail != 5 || rep.Summary.Warn != 1 {
		t.Fatalf("overall %s exit %d summary %+v", rep.Outcome, rep.ExitCode, rep.Summary)
	}
	// Behavioral failures skip performance; the WARN-only scenario would still be measured.
	for _, s := range rep.Scenarios {
		if s.Behavior.Outcome == verdict.Fail && s.Performance.Outcome != verdict.Skipped {
			t.Errorf("%s: performance should be skipped after a behavioral FAIL", s.Name)
		}
	}
	// The report must be valid, deterministic JSON without bodies.
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ada@example.com") || strings.Contains(string(data), "Ada Lovelace") {
		t.Fatal("report leaks response body content")
	}
}

func TestRunFailBeatsInconclusive(t *testing.T) {
	doc := `
version: 1
scenarios:
  - {name: unstable, method: GET, path: /api/unstable}
  - {name: users, method: GET, path: /api/users/42}
`
	rep := Run(context.Background(), mustConfig(t, doc), startFixture(t, fixture.Baseline), startFixture(t, fixture.Regressed), Options{})
	got := outcomes(rep)
	if got["unstable"] != verdict.Inconclusive || got["users"] != verdict.Fail || rep.Outcome != verdict.Fail {
		t.Fatalf("got %v overall %s", got, rep.Outcome)
	}
	if len(rep.Reasons) != 2 || !strings.HasPrefix(rep.Reasons[0], "users FAIL") || !strings.HasPrefix(rep.Reasons[1], "unstable INCONCLUSIVE") {
		t.Fatalf("reasons should list FAIL then INCONCLUSIVE: %v", rep.Reasons)
	}
}

func TestRunUnavailableBaseline(t *testing.T) {
	dead, _ := config.ParseOrigin("http://127.0.0.1:1")
	rep := Run(context.Background(), mustConfig(t, "version: 1\nscenarios: [{name: users, method: GET, path: /api/users/42}]"), dead, startFixture(t, fixture.Clean), Options{})
	if rep.Outcome != verdict.Inconclusive || rep.ExitCode != 2 {
		t.Fatalf("unavailable baseline: %s %v", rep.Outcome, rep.Reasons)
	}
}

func TestRunCanceled(t *testing.T) {
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	origin, _ := config.ParseOrigin(slow.URL)
	doc := "version: 1\nsettings: {concurrency: 2}\nscenarios:\n"
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		doc += "  - {name: " + n + ", method: GET, path: /x}\n"
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for hits.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	begin := time.Now()
	rep := Run(ctx, mustConfig(t, doc), origin, origin, Options{})
	if time.Since(begin) > 3*time.Second {
		t.Fatalf("cancellation took %s", time.Since(begin))
	}
	if !rep.Run.Canceled || rep.Outcome != verdict.Inconclusive || rep.ExitCode != verdict.ExitCanceled {
		t.Fatalf("canceled run: canceled=%v outcome=%s exit=%d", rep.Run.Canceled, rep.Outcome, rep.ExitCode)
	}
	if rep.Run.IssuedRequests >= int64(mustConfig(t, doc).PlannedRequests()) {
		t.Fatalf("cancellation should stop issuing work (issued %d)", rep.Run.IssuedRequests)
	}
	for _, s := range rep.Scenarios {
		if s.Outcome == verdict.Pass {
			t.Fatalf("%s reported PASS in a canceled run", s.Name)
		}
	}
}

// manyAdded returns a JSON object with n added keys a000..a(n-1), plus extra.
func manyAdded(n int, extra string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"a%03d":1`, i)
	}
	b.WriteString(extra + "}")
	return b.String()
}

func TestEvaluateFindingCapKeepsFail(t *testing.T) {
	// 100 WARN additions sort before the FAIL removal of /z.
	b := js(200, `{"z":1}`)
	c := js(200, manyAdded(100, ""))
	beh := Evaluate(scenario(), bURL, cURL, []replay.Response{b, c, b, c})
	if beh.Outcome != verdict.Fail {
		t.Fatalf("outcome = %s, want FAIL: a FAIL beyond the finding cap must still block", beh.Outcome)
	}
	want := compare.Counts{Total: 101, Fail: 1, Warn: 100, Retained: 100, Omitted: 1, OmittedWarn: 1}
	if beh.FindingCounts != want || beh.FindingsOmitted != 1 || len(beh.Findings) != 100 {
		t.Fatalf("counts %+v omitted %d retained %d", beh.FindingCounts, beh.FindingsOmitted, len(beh.Findings))
	}
	reason := strings.Join(beh.Reasons, " | ")
	if !strings.Contains(reason, "1 FAIL and 100 WARN") || !strings.Contains(reason, "100 retained in the report, 1 omitted") || !strings.Contains(reason, "0 FAIL, 1 WARN omitted") {
		t.Fatalf("reason misreports counts: %s", reason)
	}

	// More than 100 WARNs and nothing blocking stays WARN.
	c = js(200, manyAdded(150, `,"z":1`))
	beh = Evaluate(scenario(), bURL, cURL, []replay.Response{b, c, b, c})
	if beh.Outcome != verdict.Warn || beh.FindingCounts.Warn != 150 || beh.FindingCounts.Fail != 0 {
		t.Fatalf("150 WARNs: outcome %s counts %+v", beh.Outcome, beh.FindingCounts)
	}
}
