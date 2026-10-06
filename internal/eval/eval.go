// Package eval runs DeployGuard's seeded regression suite: known fixture
// cases with ground-truth outcomes, executed through the real engine against
// real local HTTP servers. Results report observed outcomes and measured
// latency; nothing is hard-coded.
//
// This is a check of a small seeded suite, not a claim of general detection
// accuracy.
package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/engine"
	"github.com/AddysEdge/deployguard/internal/fixture"
	"github.com/AddysEdge/deployguard/internal/report"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// Label is a case's ground truth.
type Label string

const (
	Regression    Label = "regression"    // must be detected (FAIL)
	NoRegression  Label = "no_regression" // must not FAIL
	Untrustworthy Label = "untrustworthy" // must be INCONCLUSIVE
)

// Case is one seeded scenario with its expected outcome.
type Case struct {
	ID                  string
	Title               string
	Kind                string // behavior, performance or workload
	Config              string // relative to the repository root
	Candidate           fixture.Variant
	BaselineUnavailable bool
	Expected            verdict.Outcome
	Label               Label
}

// Cases is the seeded suite.
var Cases = []Case{
	{"R1", "removed JSON field", "behavior", "examples/eval/r1-removed-field.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"R2", "changed JSON type", "behavior", "examples/eval/r2-type-change.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"R3", "changed HTTP status", "behavior", "examples/eval/r3-status-change.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"R4", "candidate server error", "behavior", "examples/eval/r4-server-error.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"R5", "large injected latency", "performance", "examples/eval/r5-latency.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"R6", "changing timestamp, ignored", "behavior", "examples/eval/r6-ignored-timestamp.yaml", fixture.Regressed, false, verdict.Pass, NoRegression},
	{"R7", "reordered JSON properties", "behavior", "examples/eval/r7-reordered-properties.yaml", fixture.Regressed, false, verdict.Pass, NoRegression},
	{"R8", "added field, default policy", "behavior", "examples/eval/r8-added-field.yaml", fixture.Regressed, false, verdict.Warn, NoRegression},
	{"R8s", "added field, strict_additions", "behavior", "examples/eval/r8-added-field-strict.yaml", fixture.Regressed, false, verdict.Fail, Regression},
	{"I1", "unstable baseline", "behavior", "examples/eval/i1-baseline-unstable.yaml", fixture.Clean, false, verdict.Inconclusive, Untrustworthy},
	{"I2", "unavailable baseline", "behavior", "examples/eval/i2-baseline-unavailable.yaml", fixture.Clean, true, verdict.Inconclusive, Untrustworthy},
	{"N1", "modest noisy latency", "performance", "examples/eval/n1-noisy-latency.yaml", fixture.Clean, false, verdict.Pass, NoRegression},
	{"E1", "full workload, clean candidate", "workload", "examples/deployguard.yaml", fixture.Clean, false, verdict.Pass, NoRegression},
	{"E2", "full workload, regressed candidate", "workload", "examples/deployguard.yaml", fixture.Regressed, false, verdict.Fail, Regression},
}

// RoundMeasurement is the measured p95 evidence of one round.
type RoundMeasurement struct {
	Scenario       string   `json:"scenario"`
	Round          int      `json:"round"`
	BaselineP95Ms  *float64 `json:"baseline_p95_ms"`
	CandidateP95Ms *float64 `json:"candidate_p95_ms"`
	DeltaP95Ms     *float64 `json:"delta_p95_ms"`
	BaselineN      int      `json:"baseline_successes"`
	CandidateN     int      `json:"candidate_successes"`
	Status         string   `json:"status"`
}

// CaseResult is one executed case.
type CaseResult struct {
	ID             string             `json:"id"`
	Title          string             `json:"title"`
	Kind           string             `json:"kind"`
	Config         string             `json:"config"`
	Candidate      string             `json:"candidate_variant"`
	Label          Label              `json:"label"`
	Expected       verdict.Outcome    `json:"expected"`
	Observed       verdict.Outcome    `json:"observed"`
	Match          bool               `json:"match"`
	Classification string             `json:"classification"` // TP, FN, TN, FP, or n/a
	DurationMs     float64            `json:"duration_ms"`
	Reasons        []string           `json:"reasons"`
	Performance    []RoundMeasurement `json:"performance,omitempty"`
	Error          string             `json:"error,omitempty"`
}

// Environment records where the suite ran.
type Environment struct {
	Date      time.Time `json:"date"`
	GoVersion string    `json:"go_version"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	CPUs      int       `json:"cpus"`
	SlowDelay string    `json:"fixture_slow_delay"`
	NoiseMax  string    `json:"fixture_noise_max"`
}

// Summary counts results. Behavioral and performance cases are kept apart.
type Summary struct {
	Cases   int                    `json:"cases"`
	Matched int                    `json:"matched"`
	ByKind  map[string]KindSummary `json:"by_kind"`
}

// KindSummary is the confusion count for one kind of case.
type KindSummary struct {
	Cases          int `json:"cases"`
	Matched        int `json:"matched"`
	TruePositives  int `json:"true_positives"`
	FalseNegatives int `json:"false_negatives"`
	TrueNegatives  int `json:"true_negatives"`
	FalsePositives int `json:"false_positives"`
}

// Result is the whole evaluation.
type Result struct {
	SchemaVersion int          `json:"schema_version"`
	Environment   Environment  `json:"environment"`
	Summary       Summary      `json:"summary"`
	Cases         []CaseResult `json:"cases"`
	Note          string       `json:"note"`
}

// AllMatched reports whether every case matched its expected outcome.
func (r *Result) AllMatched() bool { return r.Summary.Matched == r.Summary.Cases }

// Run executes the suite. root is the repository root (for config paths).
func Run(ctx context.Context, root string, cases []Case, log *slog.Logger) (*Result, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	res := &Result{
		SchemaVersion: 1,
		Environment: Environment{
			Date: time.Now().UTC(), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
			CPUs: runtime.NumCPU(), SlowDelay: fixture.DefaultSlowDelay.String(), NoiseMax: fixture.DefaultNoiseMax.String(),
		},
		Summary: Summary{ByKind: map[string]KindSummary{}},
		Note:    "Seeded fixture suite only; this is not a measure of general detection accuracy. Latency values are from this run on this machine.",
	}
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		cr := runCase(ctx, root, c, log)
		res.Cases = append(res.Cases, cr)
		res.Summary.Cases++
		ks := res.Summary.ByKind[c.Kind]
		ks.Cases++
		if cr.Match {
			res.Summary.Matched++
			ks.Matched++
		}
		switch cr.Classification {
		case "TP":
			ks.TruePositives++
		case "FN":
			ks.FalseNegatives++
		case "TN":
			ks.TrueNegatives++
		case "FP":
			ks.FalsePositives++
		}
		res.Summary.ByKind[c.Kind] = ks
	}
	return res, nil
}

func runCase(ctx context.Context, root string, c Case, log *slog.Logger) (cr CaseResult) {
	cr = CaseResult{ID: c.ID, Title: c.Title, Kind: c.Kind, Config: c.Config, Candidate: string(c.Candidate),
		Label: c.Label, Expected: c.Expected, Classification: "n/a"}
	begin := time.Now()
	defer func() { cr.DurationMs = float64(time.Since(begin).Milliseconds()) }()

	fail := func(err error) CaseResult {
		cr.Error = err.Error()
		cr.Observed = verdict.Outcome("ERROR")
		return cr
	}
	cfg, err := config.Load(filepath.Join(root, filepath.FromSlash(c.Config)), func(string) (string, bool) { return "", false })
	if err != nil {
		return fail(err)
	}

	var baseURL string
	if c.BaselineUnavailable {
		baseURL, err = closedPort()
	} else {
		var stop func()
		baseURL, stop, err = serve(fixture.Baseline)
		if stop != nil {
			defer stop()
		}
	}
	if err != nil {
		return fail(err)
	}
	candURL, stop, err := serve(c.Candidate)
	if err != nil {
		return fail(err)
	}
	defer stop()

	base, _ := config.ParseOrigin(baseURL)
	cand, _ := config.ParseOrigin(candURL)
	rep := engine.Run(ctx, cfg, base, cand, engine.Options{ConfigPath: c.Config, Version: "eval", Logger: log.With("case", c.ID)})

	cr.Observed = rep.Outcome
	cr.Match = rep.Outcome == c.Expected && !rep.Run.Canceled
	cr.Reasons = rep.Reasons
	cr.Performance = measurements(rep)
	detected := rep.Outcome == verdict.Fail
	switch c.Label {
	case Regression:
		cr.Classification = map[bool]string{true: "TP", false: "FN"}[detected]
	case NoRegression:
		cr.Classification = map[bool]string{true: "FP", false: "TN"}[detected]
	}
	return cr
}

func measurements(rep *report.Report) []RoundMeasurement {
	var out []RoundMeasurement
	for _, s := range rep.Scenarios {
		for _, r := range s.Performance.Rounds {
			out = append(out, RoundMeasurement{
				Scenario: s.Name, Round: r.Round, BaselineP95Ms: r.Baseline.P95Ms, CandidateP95Ms: r.Candidate.P95Ms,
				DeltaP95Ms: r.DeltaP95Ms, BaselineN: r.Baseline.Successes, CandidateN: r.Candidate.Successes, Status: string(r.Status),
			})
		}
	}
	return out
}

// serve starts a fixture variant on a free loopback port.
func serve(v fixture.Variant) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: fixture.NewHandler(fixture.Options{Variant: v}), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Default().Error("fixture server", "err", err)
		}
	}()
	return "http://" + ln.Addr().String(), func() { srv.Close() }, nil
}

// closedPort returns an origin on a loopback port that refuses connections.
func closedPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", fmt.Errorf("release port: %w", err)
	}
	return "http://" + addr, nil
}
