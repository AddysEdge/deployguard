package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/perf"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

func sample() *Report {
	p95b, p95c, delta, pct := 1.0, 150.0, 149.0, 14900.0
	return &Report{
		SchemaVersion: SchemaVersion,
		Tool:          Tool{Name: "deployguard", Version: "test"},
		Run:           Run{ID: "abc", Baseline: "http://b", Candidate: "http://c", ConfigPath: "w.yaml"},
		Outcome:       verdict.Fail, ExitCode: 1,
		Summary: Summary{Total: 2, Fail: 1, Pass: 1},
		Reasons: []string{"users FAIL: behavior FAIL: field_removed /email"},
		Scenarios: []Scenario{
			{
				Name: "users", Method: "GET", Path: "/api/users/42", Outcome: verdict.Fail,
				Behavior: Behavior{
					Outcome: verdict.Fail, Reasons: []string{"candidate differs"},
					Findings: []compare.Finding{
						{Category: compare.StatusChanged, Severity: verdict.Fail, Baseline: "200", Candidate: "404"},
						{Category: compare.FieldRemoved, Path: "/email", Severity: verdict.Fail, Baseline: "string", Candidate: "missing"},
					},
					IgnoreRules: []IgnoreRule{{Path: "/ts", Status: "unused", Note: "path not present in the baseline response"}},
				},
				Performance: perf.Skipped("not configured for this scenario"),
			},
			{
				Name: "search", Method: "GET", Path: "/api/search", Outcome: verdict.Pass,
				Behavior: Behavior{Outcome: verdict.Pass, Reasons: []string{"matched"}, Findings: []compare.Finding{}, IgnoreRules: []IgnoreRule{}},
				Performance: perf.Result{Outcome: verdict.Fail, Reasons: []string{"regression repeated"}, Rounds: []perf.Round{{
					Round: 1, Order: []string{"baseline", "candidate"}, Status: perf.RoundBreach,
					Baseline: perf.TargetStats{Target: "baseline", P95Ms: &p95b, Successes: 100}, Candidate: perf.TargetStats{Target: "candidate", P95Ms: &p95c, Successes: 100},
					DeltaP95Ms: &delta, DeltaP95Pct: &pct,
				}}},
			},
		},
	}
}

func TestWriteIsAtomicAndValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "report.json")
	if err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	// Overwrite an existing report.
	if err := Write(path, sample()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "tool", "run", "settings", "outcome", "exit_code", "summary", "reasons", "scenarios"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("report lacks %q", key)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestWriteFailure(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, sample()); err == nil {
		t.Fatal("writing over a directory must fail")
	}
}

func TestDeterministicJSON(t *testing.T) {
	a, _ := json.Marshal(sample())
	b, _ := json.Marshal(sample())
	if !bytes.Equal(a, b) {
		t.Fatal("report encoding is not deterministic")
	}
}

func TestRender(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, sample())
	out := buf.String()
	for _, want := range []string{
		"FAIL         users",
		"status_changed",
		"200 -> 404",
		"field_removed        /email string -> missing",
		"ignore /ts: UNUSED (path not present",
		"performance  SKIPPED      not configured",
		"round 1 baseline->candidate",
		"p95 1.0ms -> 150.0ms",
		"Result: FAIL (exit code 1)",
		"- users FAIL: behavior FAIL: field_removed /email",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal output lacks %q\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		for _, r := range line {
			if r > 0x7e {
				t.Fatalf("non-ASCII output: %q", line)
			}
		}
	}
}
