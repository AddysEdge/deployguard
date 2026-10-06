package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AddysEdge/deployguard/internal/fixture"
)

var binPath string

// TestMain builds the real CLI once; end-to-end tests execute that binary.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "deployguard-e2e")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(dir, "deployguard")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building deployguard:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	code           int
	stdout, stderr string
}

func runBinary(t *testing.T, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running binary: %v", err)
	}
	return result{code, out.String(), errb.String()}
}

func fixtureServer(t *testing.T, v fixture.Variant) string {
	t.Helper()
	srv := httptest.NewServer(fixture.NewHandler(fixture.Options{Variant: v, SlowDelay: 80 * time.Millisecond}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func readReport(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report not written: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if rep["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v", rep["schema_version"])
	}
	return rep
}

func example(name string) string { return filepath.Join("..", "..", "examples", name) }

func TestCLIExitCodesAndReports(t *testing.T) {
	baseline := fixtureServer(t, fixture.Baseline)
	clean := fixtureServer(t, fixture.Clean)
	regressed := fixtureServer(t, fixture.Regressed)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + ln.Addr().String()
	ln.Close()

	tests := []struct {
		name      string
		config    string
		baseline  string
		candidate string
		wantCode  int
		wantOut   string
	}{
		{"clean candidate passes", example("deployguard.yaml"), baseline, clean, 0, "PASS"},
		{"added field warns but does not block", example("eval/r8-added-field.yaml"), baseline, regressed, 0, "WARN"},
		{"regressed candidate fails", example("deployguard.yaml"), baseline, regressed, 1, "FAIL"},
		{"unavailable baseline is inconclusive", example("eval/i2-baseline-unavailable.yaml"), dead, clean, 2, "INCONCLUSIVE"},
		{"unstable baseline is inconclusive", example("eval/i1-baseline-unstable.yaml"), baseline, clean, 2, "INCONCLUSIVE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reportPath := filepath.Join(t.TempDir(), "nested", "run.json")
			r := runBinary(t, nil, "compare", "--config", tt.config, "--baseline", tt.baseline, "--candidate", tt.candidate, "--report", reportPath)
			if r.code != tt.wantCode {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, tt.wantCode, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stdout, fmt.Sprintf("Result: %s (exit code %d)", tt.wantOut, tt.wantCode)) {
				t.Fatalf("stdout lacks the verdict line:\n%s", r.stdout)
			}
			rep := readReport(t, reportPath)
			if rep["outcome"] != tt.wantOut || rep["exit_code"] != float64(tt.wantCode) {
				t.Fatalf("report outcome=%v exit_code=%v", rep["outcome"], rep["exit_code"])
			}
			// Logs belong on stderr only.
			if strings.Contains(r.stdout, "level=") || !strings.Contains(r.stderr, "run_id=") {
				t.Fatalf("logs are not separated: stdout has logs or stderr lacks them")
			}
		})
	}
}

func TestCLIUsageAndConfigErrors(t *testing.T) {
	good := example("deployguard.yaml")
	dir := t.TempDir()
	post := filepath.Join(dir, "post.yaml")
	os.WriteFile(post, []byte("version: 1\nscenarios: [{name: s, method: POST, path: /x}]\n"), 0o600)
	unknown := filepath.Join(dir, "unknown.yaml")
	os.WriteFile(unknown, []byte("version: 1\nscenarios: [{name: s, method: GET, path: /x, body: '{}'}]\n"), 0o600)

	tests := []struct {
		name    string
		args    []string
		code    int
		message string
	}{
		{"no args", nil, 3, "Usage"},
		{"unknown command", []string{"deploy"}, 3, "unknown command"},
		{"missing flags", []string{"compare"}, 3, "--config is required"},
		{"bad flag", []string{"compare", "--nope"}, 3, "flag provided but not defined"},
		{"stray argument", []string{"compare", "--config", good, "extra"}, 3, "unexpected argument"},
		{"credentials in URL", []string{"compare", "--config", good, "--baseline", "http://u:hunter2@127.0.0.1:1", "--candidate", "http://127.0.0.1:2"}, 3, "userinfo"},
		{"origin with path", []string{"compare", "--config", good, "--baseline", "http://127.0.0.1:1/api", "--candidate", "http://127.0.0.1:2"}, 3, "without a path"},
		{"missing config file", []string{"compare", "--config", filepath.Join(dir, "nope.yaml"), "--baseline", "http://127.0.0.1:1", "--candidate", "http://127.0.0.1:2"}, 3, "open config"},
		{"POST rejected", []string{"compare", "--config", post, "--baseline", "http://127.0.0.1:1", "--candidate", "http://127.0.0.1:2"}, 3, "POST is not supported in v1"},
		{"unknown field rejected", []string{"validate", "--config", unknown}, 3, "field body not found"},
		{"bad log level", []string{"compare", "--config", good, "--baseline", "http://127.0.0.1:1", "--candidate", "http://127.0.0.1:2", "--log-level", "loud"}, 3, "--log-level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runBinary(t, nil, tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr, tt.message) {
				t.Fatalf("exit %d (want %d), stderr %q should contain %q", r.code, tt.code, r.stderr, tt.message)
			}
			if strings.Contains(r.stderr, "hunter2") {
				t.Fatal("credentials echoed")
			}
		})
	}
}

func TestCLIHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"help", "compare"}, {"compare", "--help"}, {"validate", "-h"}, {"version"}} {
		r := runBinary(t, nil, args...)
		if r.code != 0 || r.stdout == "" {
			t.Fatalf("%v: exit %d stdout %q", args, r.code, r.stdout)
		}
	}
	r := runBinary(t, nil, "compare", "--help")
	for _, want := range []string{"--baseline", "--candidate", "--report", "--config"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("compare --help lacks %s", want)
		}
	}
	r = runBinary(t, nil, "validate", "--config", example("deployguard.yaml"))
	if r.code != 0 || !strings.Contains(r.stdout, "valid") {
		t.Fatalf("validate: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestCLIReportWriteFailureIsConspicuous(t *testing.T) {
	baseline := fixtureServer(t, fixture.Baseline)
	clean := fixtureServer(t, fixture.Clean)
	dir := t.TempDir() // a directory cannot be overwritten by the report
	r := runBinary(t, nil, "compare", "--config", example("eval/r1-removed-field.yaml"), "--baseline", baseline, "--candidate", clean, "--report", dir)
	if r.code != 3 || !strings.Contains(r.stderr, "ERROR: the JSON report could not be written") {
		t.Fatalf("exit %d stderr %s", r.code, r.stderr)
	}
}

func TestCLISecretsAreSentButNeverPrinted(t *testing.T) {
	const secret = "e2e-secret-7c1f9a"    // header value, from the environment
	const querySecret = "q-secret-5d2e8b" // literal query value in the workload
	type seen struct{ auth, apiKey, token string }
	handler := fixture.NewHandler(fixture.Options{Variant: fixture.Baseline})
	var got [2]atomic.Value
	newServer := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got[i].Store(seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.URL.Query().Get("token")})
			if r.URL.Path == "/api/login-redirect" {
				// Echo the secret into a same-origin Location query.
				w.Header().Set("Location", "/api/next?token="+r.URL.Query().Get("token"))
				w.WriteHeader(http.StatusFound)
				return
			}
			handler.ServeHTTP(w, r)
		}))
	}
	srv, srv2 := newServer(0), newServer(1)
	defer srv.Close()
	defer srv2.Close()

	cfg := filepath.Join(t.TempDir(), "secret.yaml")
	os.WriteFile(cfg, []byte(`version: 1
scenarios:
  - name: users
    method: GET
    path: /api/users/42
    query:
      token: `+querySecret+`
      page: "2"
    headers:
      Authorization: "Bearer ${DG_E2E_TOKEN}"
      X-Api-Key: "${DG_E2E_TOKEN}"
  - name: login-redirect
    method: GET
    path: /api/login-redirect
    query:
      token: `+querySecret+`
`), 0o600)
	reportPath := filepath.Join(t.TempDir(), "r.json")
	r := runBinary(t, []string{"DG_E2E_TOKEN=" + secret}, "compare", "--config", cfg, "--baseline", srv.URL, "--candidate", srv2.URL, "--report", reportPath, "--log-level", "debug")
	if r.code != 0 {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	// The real requests carry the original values on both targets...
	for i, name := range []string{"baseline", "candidate"} {
		s, _ := got[i].Load().(seen)
		if s.token != querySecret {
			t.Fatalf("%s did not receive the original query value (got %q)", name, s.token)
		}
	}
	// ...but no output stream or the report contains them.
	data, _ := os.ReadFile(reportPath)
	for where, s := range map[string]string{"stdout": r.stdout, "stderr": r.stderr, "report": string(data)} {
		for _, planted := range []string{secret, querySecret} {
			if strings.Contains(s, planted) {
				t.Fatalf("planted secret %q leaked to %s", planted, where)
			}
		}
	}
	// Query parameter names and multiplicity are kept; values are redacted.
	var rep struct {
		Scenarios []struct {
			Name     string              `json:"name"`
			Query    map[string][]string `json:"query"`
			Behavior struct {
				Observations []struct {
					Location string `json:"location"`
				} `json:"observations"`
			} `json:"behavior"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	q := rep.Scenarios[0].Query
	if len(q) != 2 || len(q["token"]) != 1 || q["token"][0] != "<redacted>" || q["page"][0] != "<redacted>" {
		t.Fatalf("report query should keep names and redact values: %v", q)
	}
	if loc := rep.Scenarios[1].Behavior.Observations[0].Location; loc != "/api/next?<redacted>" {
		t.Fatalf("redirect Location display = %q", loc)
	}
	// Headers resolved from the environment are sent on the wire (checked in a
	// single-scenario run so the last recorded request is unambiguous).
	headerOnly := filepath.Join(t.TempDir(), "header.yaml")
	os.WriteFile(headerOnly, []byte(`version: 1
scenarios:
  - name: users
    method: GET
    path: /api/users/42
    headers:
      Authorization: "Bearer ${DG_E2E_TOKEN}"
      X-Api-Key: "${DG_E2E_TOKEN}"
`), 0o600)
	r = runBinary(t, []string{"DG_E2E_TOKEN=" + secret}, "compare", "--config", headerOnly, "--baseline", srv.URL, "--candidate", srv2.URL, "--log-level", "debug")
	for i := range got {
		if s, _ := got[i].Load().(seen); r.code != 0 || s.auth != "Bearer "+secret || s.apiKey != secret {
			t.Fatalf("target %d did not receive resolved headers (exit %d, %+v)", i, r.code, s)
		}
	}
	if strings.Contains(r.stdout+r.stderr, secret) {
		t.Fatal("header secret leaked to output")
	}
	cfg = headerOnly

	// A missing variable fails before any request is sent.
	r = runBinary(t, []string{"DG_E2E_TOKEN="}, "validate", "--config", cfg)
	if r.code != 0 {
		t.Fatalf("an empty but set variable is valid: %d %s", r.code, r.stderr)
	}
	cmd := exec.Command(binPath, "validate", "--config", cfg)
	cmd.Env = withoutVar(os.Environ(), "DG_E2E_TOKEN")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "DG_E2E_TOKEN is not set") {
		t.Fatalf("missing env var should fail validation: %s", out)
	}
}

func withoutVar(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// TestRunCanceledWritesReport cancels an in-process run (the same code path
// SIGINT/SIGTERM trigger in main) and checks the exit code and report.
func TestRunCanceledWritesReport(t *testing.T) {
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer slow.Close()
	slow2 := httptest.NewServer(slow.Config.Handler) // a distinct origin with the same behavior
	defer slow2.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for hits.Load() < 1 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	reportPath := filepath.Join(t.TempDir(), "canceled.json")
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"compare", "--config", example("deployguard.yaml"), "--baseline", slow.URL, "--candidate", slow2.URL, "--report", reportPath, "--log-level", "error"}, &stdout, &stderr)
	if code != 130 {
		t.Fatalf("exit %d, want 130\n%s\n%s", code, stdout.String(), stderr.String())
	}
	rep := readReport(t, reportPath)
	run := rep["run"].(map[string]any)
	if run["canceled"] != true || rep["outcome"] != "INCONCLUSIVE" || rep["exit_code"] != float64(130) {
		t.Fatalf("canceled report: outcome=%v canceled=%v exit=%v", rep["outcome"], run["canceled"], rep["exit_code"])
	}
}

// capServer serves /cap-fail (baseline {"z":1}; candidate 100 added fields
// and no "z") and /cap-warn (candidate keeps "z" and adds 150 fields).
func capServer(t *testing.T, candidate bool) string {
	t.Helper()
	added := func(n int, extra string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"a%03d":1`, i)
		}
		return b.String() + extra + "}"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !candidate:
			fmt.Fprint(w, `{"z":1}`)
		case r.URL.Path == "/cap-fail":
			fmt.Fprint(w, added(100, ""))
		default:
			fmt.Fprint(w, added(150, `,"z":1`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCLIFindingCapCannotHideFail(t *testing.T) {
	baseline, candidate := capServer(t, false), capServer(t, true)
	dir := t.TempDir()
	write := func(name, path string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("version: 1\nscenarios: [{name: cap, method: GET, path: "+path+"}]\n"), 0o600)
		return p
	}
	type counts struct {
		Total, Fail, Warn, Retained, Omitted int
		OmittedFail                          int `json:"omitted_fail"`
		OmittedWarn                          int `json:"omitted_warn"`
	}
	type finding struct{ Category, Path, Severity string }
	load := func(path string) (string, counts, []finding, int) {
		var rep struct {
			Outcome   string `json:"outcome"`
			Scenarios []struct {
				Behavior struct {
					Findings        []finding `json:"findings"`
					FindingsOmitted int       `json:"findings_omitted"`
					FindingCounts   counts    `json:"finding_counts"`
				} `json:"behavior"`
			} `json:"scenarios"`
		}
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &rep) != nil {
			t.Fatalf("unreadable report %s: %v", path, err)
		}
		b := rep.Scenarios[0].Behavior
		return rep.Outcome, b.FindingCounts, b.Findings, b.FindingsOmitted
	}

	// 100 WARN additions sort before the FAIL removal of /z: must still exit 1.
	failCfg := write("fail.yaml", "/cap-fail")
	var first []finding
	for run := 0; run < 2; run++ {
		reportPath := filepath.Join(dir, fmt.Sprintf("fail-%d.json", run))
		r := runBinary(t, nil, "compare", "--config", failCfg, "--baseline", baseline, "--candidate", candidate, "--report", reportPath)
		if r.code != 1 {
			t.Fatalf("exit %d, want 1: a FAIL beyond the finding cap must block\n%s", r.code, r.stdout)
		}
		outcome, c, fs, omitted := load(reportPath)
		want := counts{Total: 101, Fail: 1, Warn: 100, Retained: 100, Omitted: 1, OmittedWarn: 1}
		if outcome != "FAIL" || c != want || len(fs) != 100 || omitted != 1 {
			t.Fatalf("outcome %s counts %+v retained %d omitted %d", outcome, c, len(fs), omitted)
		}
		if last := fs[len(fs)-1]; last.Path != "/z" || last.Severity != "FAIL" {
			t.Fatalf("the blocking finding must be retained: last = %+v", last)
		}
		if run == 0 {
			first = fs
		} else if fmt.Sprint(fs) != fmt.Sprint(first) {
			t.Fatal("retained findings are not deterministic across runs")
		}
		if !strings.Contains(r.stdout, "not retained (cap of 100 per comparison) (0 FAIL, 1 WARN)") || !strings.Contains(r.stdout, "cap FAIL: behavior FAIL: field_removed /z") {
			t.Fatalf("terminal output should explain omitted findings and name the FAIL:\n%s", r.stdout)
		}
	}

	// More than 100 WARNs and no FAIL: WARN, exit 0.
	reportPath := filepath.Join(dir, "warn.json")
	r := runBinary(t, nil, "compare", "--config", write("warn.yaml", "/cap-warn"), "--baseline", baseline, "--candidate", candidate, "--report", reportPath)
	outcome, c, fs, _ := load(reportPath)
	if r.code != 0 || outcome != "WARN" || c != (counts{Total: 150, Warn: 150, Retained: 100, Omitted: 50, OmittedWarn: 50}) || len(fs) != 100 {
		t.Fatalf("exit %d outcome %s counts %+v", r.code, outcome, c)
	}
}

func TestCLIRejectsEquivalentOrigins(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfg := example("deployguard.yaml")
	for _, pair := range [][2]string{
		{srv.URL, srv.URL},
		{srv.URL, srv.URL + "/"},
		{"http://example.test", "http://EXAMPLE.test:80"},
		{"https://Example.Test", "https://example.test:443"},
	} {
		r := runBinary(t, nil, "compare", "--config", cfg, "--baseline", pair[0], "--candidate", pair[1])
		if r.code != 3 || !strings.Contains(r.stderr, "are the same origin") || !strings.Contains(r.stderr, "no requests were sent") {
			t.Fatalf("%v: exit %d stderr %s", pair, r.code, r.stderr)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) were sent despite rejection", n)
	}

	// Distinct ports on the same host are different origins.
	a, b := fixtureServer(t, fixture.Baseline), fixtureServer(t, fixture.Clean)
	if r := runBinary(t, nil, "compare", "--config", example("eval/r1-removed-field.yaml"), "--baseline", a, "--candidate", b); r.code != 0 {
		t.Fatalf("distinct ports should be accepted: exit %d %s", r.code, r.stderr)
	}
	// validate takes no origins and is unaffected.
	if r := runBinary(t, nil, "validate", "--config", cfg); r.code != 0 {
		t.Fatalf("validate: exit %d", r.code)
	}
}
