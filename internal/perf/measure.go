package perf

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// Measure runs the two measured rounds for one scenario and decides the
// performance outcome. validStatus is the status a sample must return to
// count as a success (the scenario's expect_status, or the status the
// baseline returned during functional checks).
func Measure(ctx context.Context, base, cand *replay.Target, req replay.Request, p config.Performance, validStatus int, log *slog.Logger) Result {
	res := Result{Policy: &Policy{
		Rounds: config.MeasuredRounds, Warmup: p.Warmup, Samples: p.Samples, Concurrency: p.Concurrency,
		P95RelativePct: p.P95RelativePct, P95AbsoluteMs: p.P95AbsoluteMs, Gate: p.Gate,
		PercentileMethod: PercentileMethod, ValidStatus: validStatus,
	}}
	orders := [config.MeasuredRounds][2]*replay.Target{{base, cand}, {cand, base}}
	for i, order := range orders {
		round := Round{Round: i + 1, Order: []string{order[0].Name, order[1].Name}}
		for _, t := range order {
			log.Debug("measuring target block", "round", round.Round, "target", t.Name, "warmup", p.Warmup, "samples", p.Samples, "concurrency", p.Concurrency)
			stats := runBlock(ctx, t, req, p, validStatus)
			if t == base {
				round.Baseline = stats
			} else {
				round.Candidate = stats
			}
		}
		if ctx.Err() != nil {
			res.Outcome = verdict.Inconclusive
			res.Reasons = []string{"run canceled during performance measurement"}
			return res
		}
		round.Classify(p)
		log.Info("performance round complete", "round", round.Round, "status", round.Status,
			"baseline_p95_ms", deref(round.Baseline.P95Ms), "candidate_p95_ms", deref(round.Candidate.P95Ms),
			"baseline_errors", round.Baseline.Errors(), "candidate_errors", round.Candidate.Errors())
		res.Rounds = append(res.Rounds, round)
	}
	res.Aggregate = &Aggregate{
		Baseline:  aggregate(res.Rounds, func(r Round) TargetStats { return r.Baseline }),
		Candidate: aggregate(res.Rounds, func(r Round) TargetStats { return r.Candidate }),
	}
	res.Outcome, res.Reasons = Decide(res.Rounds, p)
	return res
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// runBlock runs warm-up then measured attempts against one target.
// Timeouts and other failures are errors, never discarded slow samples.
func runBlock(ctx context.Context, t *replay.Target, req replay.Request, p config.Performance, validStatus int) TargetStats {
	stats := TargetStats{Target: t.Name, ErrorKinds: map[string]int{}}
	var mu sync.Mutex
	record := func(r replay.Response, warmup bool) bool {
		kind := ""
		switch {
		case r.Failure != nil && r.Failure.Kind == replay.FailCanceled:
			return false // the run is stopping; not the target's fault
		case r.Failure != nil:
			kind = string(r.Failure.Kind)
		case r.Status != validStatus:
			kind = fmt.Sprintf("status_%d", r.Status)
		}
		mu.Lock()
		defer mu.Unlock()
		if warmup {
			stats.WarmupAttempts++
		} else {
			stats.Attempts++
		}
		if kind == "" {
			return true
		}
		switch {
		case warmup:
			stats.WarmupErrors++
			kind = "warmup_" + kind
		case r.Failure != nil:
			stats.TransportErrors++
		default:
			stats.StatusErrors++
		}
		stats.ErrorKinds[kind]++
		return false
	}

	replay.ForEach(ctx, p.Warmup, p.Concurrency, func(ctx context.Context, _ int) {
		record(t.Do(ctx, req, false), true)
	})

	durations := make([]time.Duration, p.Samples)
	ok := make([]bool, p.Samples)
	start := time.Now()
	replay.ForEach(ctx, p.Samples, p.Concurrency, func(ctx context.Context, i int) {
		r := t.Do(ctx, req, false)
		if record(r, false) {
			durations[i], ok[i] = r.Duration, true
		}
	})
	stats.WallMs = Ms(time.Since(start))

	valid := durations[:0]
	for i, d := range durations {
		if ok[i] {
			valid = append(valid, d)
		}
	}
	stats.setDurations(valid)
	if len(stats.ErrorKinds) == 0 {
		stats.ErrorKinds = nil
	}
	return stats
}
