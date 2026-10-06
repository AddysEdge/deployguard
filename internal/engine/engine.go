// Package engine orchestrates a DeployGuard run: functional observations of
// every scenario (bounded concurrency), behavioral verdicts, opt-in
// performance measurement for behaviorally healthy scenarios, and the final
// outcome.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"time"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/perf"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/report"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// Options configure a run.
type Options struct {
	ConfigPath string
	Version    string
	RunID      string       // generated when empty
	Logger     *slog.Logger // discarded when nil
}

// NewRunID returns a random 16-hex-character run identifier.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Run executes the workload against both targets and returns the report. It
// never returns PASS for work it did not do: canceled runs are marked as such
// and cannot produce an approving outcome.
func Run(ctx context.Context, cfg *config.Config, baseline, candidate *url.URL, opts Options) *report.Report {
	if opts.RunID == "" {
		opts.RunID = NewRunID()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	log := opts.Logger.With("run_id", opts.RunID)

	maxConns := cfg.Settings.Concurrency
	for _, sc := range cfg.Scenarios {
		if sc.Performance != nil {
			maxConns = max(maxConns, sc.Performance.Concurrency)
		}
	}
	base := replay.NewTarget("baseline", baseline, maxConns)
	cand := replay.NewTarget("candidate", candidate, maxConns)
	defer base.Close()
	defer cand.Close()

	started := time.Now()
	rep := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Tool:          report.Tool{Name: "deployguard", Version: opts.Version},
		Run: report.Run{
			ID: opts.RunID, StartedAt: started.UTC(), ConfigPath: opts.ConfigPath, ConfigVersion: cfg.Version,
			Baseline: baseline.String(), Candidate: candidate.String(), PlannedRequests: cfg.PlannedRequests(),
		},
		Settings: report.Settings{
			TimeoutMs: cfg.Settings.Timeout.Milliseconds(), MaxResponseBytes: cfg.Settings.MaxResponseBytes,
			Concurrency: cfg.Settings.Concurrency, MaxTotalRequests: cfg.Settings.MaxTotalRequests,
		},
		Reasons:   []string{},
		Scenarios: []report.Scenario{},
	}

	// Phase 1: functional observations, scenarios in parallel up to the
	// global concurrency; each scenario's four requests are sequential.
	n := len(cfg.Scenarios)
	behaviors := make([]report.Behavior, n)
	checked := make([]bool, n)
	flog := log.With("phase", "functional")
	flog.Info("phase started", "scenarios", n, "concurrency", cfg.Settings.Concurrency)
	phaseStart := time.Now()
	replay.ForEach(ctx, n, cfg.Settings.Concurrency, func(ctx context.Context, i int) {
		sc := cfg.Scenarios[i]
		behaviors[i] = observe(ctx, base, cand, sc, cfg.Settings.MaxResponseBytes)
		checked[i] = true
		flog.Info("scenario checked", "scenario", sc.Name, "behavior", behaviors[i].Outcome)
	})
	for i := range behaviors {
		if !checked[i] {
			behaviors[i] = report.Behavior{
				Outcome: verdict.Inconclusive, Reasons: []string{"run canceled before this scenario was checked"},
				Findings: []compare.Finding{}, IgnoreRules: ignoreReport(cfg.Scenarios[i], nil, false, nil, nil),
			}
		}
	}
	flog.Info("phase finished", "duration_ms", perf.Ms(time.Since(phaseStart)), "requests", base.Issued()+cand.Issued())

	// Phase 2: performance, one scenario at a time so measurements of
	// different scenarios never overlap.
	plog := log.With("phase", "performance")
	for i, sc := range cfg.Scenarios {
		beh := behaviors[i]
		var pr perf.Result
		switch {
		case sc.Performance == nil:
			pr = perf.Skipped("not configured for this scenario")
		case ctx.Err() != nil:
			pr = perf.Skipped("run canceled before performance measurement")
		case beh.Outcome == verdict.Fail || beh.Outcome == verdict.Inconclusive:
			pr = perf.Skipped(fmt.Sprintf("behavioral outcome is %s; latency is only measured when behavior is PASS or WARN", beh.Outcome))
		default:
			valid := sc.ExpectStatus
			if valid == 0 {
				valid = beh.Observations[0].Status
			}
			slg := plog.With("scenario", sc.Name)
			slg.Info("measuring", "warmup", sc.Performance.Warmup, "samples", sc.Performance.Samples, "concurrency", sc.Performance.Concurrency)
			req := replay.RequestFor(sc, cfg.Settings.MaxResponseBytes)
			pr = perf.Measure(ctx, base, cand, req, *sc.Performance, valid, slg)
			slg.Info("measured", "performance", pr.Outcome)
		}
		rep.Scenarios = append(rep.Scenarios, report.Scenario{
			Name: sc.Name, Method: sc.Method, Path: sc.Path, Query: queryOrNil(sc.Query),
			Outcome: verdict.Worst(beh.Outcome, pr.Outcome), Behavior: beh, Performance: pr,
		})
	}

	// Final outcome.
	canceled := ctx.Err() != nil
	final := verdict.Pass
	for _, s := range rep.Scenarios {
		rep.Summary.Add(s.Outcome)
		final = verdict.Worst(final, s.Outcome)
	}
	if canceled {
		final = verdict.Worst(final, verdict.Inconclusive)
		rep.Reasons = append(rep.Reasons, "run canceled by the user before completion")
	}
	rep.Reasons = append(rep.Reasons, scenarioReasons(rep.Scenarios)...)
	if final == verdict.Pass || (final == verdict.Warn && !canceled) {
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("no scenario failed or was inconclusive (%d PASS, %d WARN); WARN does not block release", rep.Summary.Pass, rep.Summary.Warn))
	}
	rep.Outcome = final
	rep.ExitCode = verdict.ExitCode(final)
	if canceled {
		rep.ExitCode = verdict.ExitCanceled
	}
	rep.Run.Canceled = canceled
	finished := time.Now()
	rep.Run.FinishedAt = finished.UTC()
	rep.Run.DurationMs = perf.Ms(finished.Sub(started))
	rep.Run.IssuedRequests = base.Issued() + cand.Issued()
	log.Info("run finished", "outcome", final, "duration_ms", rep.Run.DurationMs, "requests", rep.Run.IssuedRequests, "canceled", canceled)
	return rep
}

// scenarioReasons lists non-PASS scenarios, most severe first.
func scenarioReasons(scenarios []report.Scenario) []string {
	order := map[verdict.Outcome]int{verdict.Fail: 0, verdict.Inconclusive: 1, verdict.Warn: 2}
	var picked []report.Scenario
	for _, s := range scenarios {
		if _, ok := order[s.Outcome]; ok {
			picked = append(picked, s)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return order[picked[i].Outcome] < order[picked[j].Outcome] })
	out := make([]string, 0, len(picked))
	for _, s := range picked {
		out = append(out, report.Reason(s))
	}
	return out
}

func queryOrNil(q url.Values) map[string][]string {
	if len(q) == 0 {
		return nil
	}
	return q
}
