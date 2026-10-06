// Package perf implements DeployGuard's opt-in latency gate.
//
// Measurement runs two rounds. Round 1 measures the baseline then the
// candidate; round 2 measures the candidate then the baseline. Each target
// block has a warm-up (excluded from metrics) followed by the configured
// number of measured attempts at a fixed concurrency. Baseline and candidate
// are never loaded at the same time.
//
// A round breaches only when the candidate p95 exceeds the baseline p95 by
// more than BOTH the relative and the absolute threshold. These are
// operational thresholds, not statistical significance tests.
package perf

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// PercentileMethod documents how percentiles are computed.
const PercentileMethod = "nearest-rank"

// Percentile returns the nearest-rank q-th percentile (0 < q <= 100) of an
// ascending slice: the smallest value such that at least q% of samples are
// less than or equal to it, i.e. sorted[ceil(q/100*n)-1].
func Percentile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(q / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// Ms converts a duration to milliseconds rounded to microsecond precision.
func Ms(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000
}

func msPtr(d time.Duration) *float64 { v := Ms(d); return &v }

// TargetStats summarizes one target block (warm-up plus measured attempts).
type TargetStats struct {
	Target          string         `json:"target"`
	WarmupAttempts  int            `json:"warmup_attempts"`
	WarmupErrors    int            `json:"warmup_errors"`
	Attempts        int            `json:"attempts"`
	Successes       int            `json:"successes"`
	TransportErrors int            `json:"transport_errors"`
	StatusErrors    int            `json:"status_errors"`
	ErrorKinds      map[string]int `json:"error_kinds,omitempty"`
	P50Ms           *float64       `json:"p50_ms"` // over successful measured attempts; null when there are none
	P95Ms           *float64       `json:"p95_ms"`
	WallMs          float64        `json:"wall_ms"` // wall-clock time of the measured attempts

	durations []time.Duration // successful measured durations, ascending
}

// Errors counts every failed attempt, warm-up included.
func (s TargetStats) Errors() int { return s.WarmupErrors + s.TransportErrors + s.StatusErrors }

// NewTargetStats computes percentiles from successful measured durations.
func NewTargetStats(target string, durations []time.Duration) TargetStats {
	s := TargetStats{Target: target}
	s.setDurations(durations)
	return s
}

func (s *TargetStats) setDurations(d []time.Duration) {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	s.durations = sorted
	s.Successes = len(sorted)
	if len(sorted) > 0 {
		s.P50Ms = msPtr(Percentile(sorted, 50))
		s.P95Ms = msPtr(Percentile(sorted, 95))
	} else {
		s.P50Ms, s.P95Ms = nil, nil
	}
}

// RoundStatus classifies one measured round.
type RoundStatus string

const (
	RoundOK              RoundStatus = "ok"
	RoundBreach          RoundStatus = "breach"
	RoundCandidateErrors RoundStatus = "candidate_errors"
	RoundBaselineErrors  RoundStatus = "baseline_errors"
	RoundIncomplete      RoundStatus = "incomplete"
)

// Round is one measured round.
type Round struct {
	Round       int         `json:"round"`
	Order       []string    `json:"order"`
	Baseline    TargetStats `json:"baseline"`
	Candidate   TargetStats `json:"candidate"`
	DeltaP95Ms  *float64    `json:"delta_p95_ms"`
	DeltaP95Pct *float64    `json:"delta_p95_pct"`
	Status      RoundStatus `json:"status"`
}

// Classify sets the round's delta and status. A round is latency-gated only
// when every warm-up and measured attempt for both targets succeeded.
func (r *Round) Classify(p config.Performance) {
	switch {
	case r.Baseline.Attempts < p.Samples || r.Candidate.Attempts < p.Samples:
		r.Status = RoundIncomplete
		return
	case r.Baseline.Errors() > 0:
		r.Status = RoundBaselineErrors
		return
	case r.Candidate.Errors() > 0:
		r.Status = RoundCandidateErrors
		return
	}
	bp := Percentile(r.Baseline.durations, 95)
	cp := Percentile(r.Candidate.durations, 95)
	delta := Ms(cp) - Ms(bp)
	r.DeltaP95Ms = &delta
	relExceeded := true // a zero baseline p95 makes any increase infinitely large
	if bp > 0 {
		pct := math.Round(delta/Ms(bp)*100*10) / 10
		r.DeltaP95Pct = &pct
		relExceeded = delta/Ms(bp)*100 > p.P95RelativePct
	}
	if delta > p.P95AbsoluteMs && relExceeded {
		r.Status = RoundBreach
	} else {
		r.Status = RoundOK
	}
}

// Policy is the reported, effective performance policy.
type Policy struct {
	Rounds           int             `json:"rounds"`
	Warmup           int             `json:"warmup"`
	Samples          int             `json:"samples"`
	Concurrency      int             `json:"concurrency"`
	P95RelativePct   float64         `json:"p95_relative_pct"`
	P95AbsoluteMs    float64         `json:"p95_absolute_ms"`
	Gate             verdict.Outcome `json:"gate"`
	PercentileMethod string          `json:"percentile_method"`
	ValidStatus      int             `json:"valid_status"`
}

// AggregateStats combines both rounds for one target (informational only;
// the gate uses per-round values).
type AggregateStats struct {
	WarmupAttempts int      `json:"warmup_attempts"`
	Attempts       int      `json:"attempts"` // measured attempts, as in TargetStats
	Successes      int      `json:"successes"`
	Errors         int      `json:"errors"` // warm-up and measured
	P50Ms          *float64 `json:"p50_ms"`
	P95Ms          *float64 `json:"p95_ms"`
}

// Aggregate holds per-target aggregates across rounds.
type Aggregate struct {
	Baseline  AggregateStats `json:"baseline"`
	Candidate AggregateStats `json:"candidate"`
}

// Result is the performance sub-result of a scenario.
type Result struct {
	Outcome   verdict.Outcome `json:"outcome"`
	Reasons   []string        `json:"reasons"`
	Policy    *Policy         `json:"policy,omitempty"`
	Rounds    []Round         `json:"rounds,omitempty"`
	Aggregate *Aggregate      `json:"aggregate,omitempty"`
}

// Skipped returns a SKIPPED result that carries no evidence.
func Skipped(reason string) Result {
	return Result{Outcome: verdict.Skipped, Reasons: []string{reason}}
}

func aggregate(rounds []Round, pick func(Round) TargetStats) AggregateStats {
	var a AggregateStats
	var all []time.Duration
	for _, r := range rounds {
		s := pick(r)
		a.WarmupAttempts += s.WarmupAttempts
		a.Attempts += s.Attempts
		a.Errors += s.Errors()
		all = append(all, s.durations...)
	}
	s := NewTargetStats("", all)
	a.Successes, a.P50Ms, a.P95Ms = s.Successes, s.P50Ms, s.P95Ms
	return a
}

// Decide turns classified rounds into an outcome and reasons.
func Decide(rounds []Round, p config.Performance) (verdict.Outcome, []string) {
	var reasons []string
	var bad []string
	for _, r := range rounds {
		switch r.Status {
		case RoundIncomplete:
			return verdict.Inconclusive, []string{fmt.Sprintf("round %d did not complete the required %d measured attempts per target", r.Round, p.Samples)}
		case RoundBaselineErrors:
			reasons = append(reasons, fmt.Sprintf("round %d: baseline had %d error(s) during warm-up or measurement, so latency cannot be compared", r.Round, r.Baseline.Errors()))
		}
	}
	if len(reasons) > 0 {
		return verdict.Inconclusive, reasons
	}
	for _, r := range rounds {
		switch r.Status {
		case RoundBreach:
			bad = append(bad, fmt.Sprintf("round %d: candidate p95 %.1fms vs baseline %.1fms (+%.1fms, %s) exceeds both thresholds (> %gms and > %g%%)",
				r.Round, *r.Candidate.P95Ms, *r.Baseline.P95Ms, *r.DeltaP95Ms, fmtPct(r.DeltaP95Pct), p.P95AbsoluteMs, p.P95RelativePct))
		case RoundCandidateErrors:
			bad = append(bad, fmt.Sprintf("round %d: candidate had %d error(s) (%s) while the baseline had none; its latency is not judged from the successful subset",
				r.Round, r.Candidate.Errors(), kinds(r.Candidate.ErrorKinds)))
		}
	}
	switch len(bad) {
	case 0:
		var parts []string
		for _, r := range rounds {
			parts = append(parts, fmt.Sprintf("round %d %+.1fms (%s)", r.Round, *r.DeltaP95Ms, fmtPct(r.DeltaP95Pct)))
		}
		return verdict.Pass, []string{"candidate p95 within thresholds in every round: " + strings.Join(parts, ", ")}
	case len(rounds):
		return p.Gate, append([]string{fmt.Sprintf("regression repeated in all %d rounds (gate: %s)", len(rounds), strings.ToLower(string(p.Gate)))}, bad...)
	default:
		return verdict.Warn, append([]string{"suspected, non-repeatable regression: only one round exceeded the gate, so it is reported as WARN"}, bad...)
	}
}

func fmtPct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *p)
}

func kinds(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
