package perf

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

func ms(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func seq(from, to int) []time.Duration {
	var out []time.Duration
	for i := from; i <= to; i++ {
		out = append(out, time.Duration(i)*time.Millisecond)
	}
	return out
}

func TestPercentileNearestRank(t *testing.T) {
	tests := []struct {
		in   []time.Duration
		q    float64
		want time.Duration
	}{
		{nil, 95, 0},
		{ms(7), 50, 7 * time.Millisecond},
		{ms(7), 95, 7 * time.Millisecond},
		{seq(1, 100), 50, 50 * time.Millisecond},
		{seq(1, 100), 95, 95 * time.Millisecond},
		{seq(1, 100), 100, 100 * time.Millisecond},
		{seq(1, 20), 95, 19 * time.Millisecond}, // ceil(0.95*20)=19
		{seq(1, 10), 95, 10 * time.Millisecond}, // ceil(9.5)=10
		{seq(1, 10), 50, 5 * time.Millisecond},
		{seq(1, 200), 95, 190 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := Percentile(tt.in, tt.q); got != tt.want {
			t.Errorf("Percentile(n=%d, %g) = %s, want %s", len(tt.in), tt.q, got, tt.want)
		}
	}
}

func policy(rel, abs float64, gate verdict.Outcome) config.Performance {
	return config.Performance{Warmup: 0, Samples: 100, Concurrency: 1, P95RelativePct: rel, P95AbsoluteMs: abs, Gate: gate}
}

// stats builds a 100-sample block whose p95 is p95ms.
func stats(target string, p95ms int) TargetStats {
	d := make([]time.Duration, 100)
	for i := range d {
		d[i] = time.Duration(p95ms) * time.Millisecond
	}
	s := NewTargetStats(target, d)
	s.Attempts = 100
	return s
}

func round(n, base, cand int, p config.Performance) Round {
	r := Round{Round: n, Baseline: stats("baseline", base), Candidate: stats("candidate", cand)}
	r.Classify(p)
	return r
}

func TestClassifyRequiresBothThresholds(t *testing.T) {
	p := policy(50, 20, verdict.Fail)
	tests := []struct {
		name       string
		base, cand int
		want       RoundStatus
	}{
		{"no change", 10, 10, RoundOK},
		{"faster", 10, 5, RoundOK},
		{"relative only (noise on a fast endpoint)", 2, 6, RoundOK}, // +200% but only +4ms
		{"absolute only (slow endpoint)", 400, 450, RoundOK},        // +50ms but only +12.5%
		{"exactly at thresholds is not a breach", 40, 60, RoundOK},  // +20ms, +50%
		{"both exceeded", 10, 120, RoundBreach},                     // +110ms, +1100%
		{"both exceeded just over", 40, 61, RoundBreach},            // +21ms, +52.5%
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := round(1, tt.base, tt.cand, p)
			if r.Status != tt.want {
				t.Fatalf("status = %s, want %s (delta %.1fms %s)", r.Status, tt.want, *r.DeltaP95Ms, fmtPct(r.DeltaP95Pct))
			}
		})
	}
}

func TestClassifyErrorsAndIncomplete(t *testing.T) {
	p := policy(10, 1, verdict.Fail)
	r := Round{Baseline: stats("baseline", 5), Candidate: stats("candidate", 500)}
	r.Baseline.TransportErrors = 1
	r.Candidate.StatusErrors = 3
	r.Classify(p)
	if r.Status != RoundBaselineErrors {
		t.Fatalf("baseline errors must dominate, got %s", r.Status)
	}
	r = Round{Baseline: stats("baseline", 5), Candidate: stats("candidate", 5)}
	r.Candidate.WarmupErrors = 1
	r.Classify(p)
	if r.Status != RoundCandidateErrors || r.DeltaP95Ms != nil {
		t.Fatalf("candidate warm-up error must make the round errored without a latency delta: %+v", r)
	}
	r = Round{Baseline: stats("baseline", 5), Candidate: stats("candidate", 5)}
	r.Candidate.Attempts = 40
	r.Classify(p)
	if r.Status != RoundIncomplete {
		t.Fatalf("want incomplete, got %s", r.Status)
	}
}

func TestDecide(t *testing.T) {
	failGate := policy(50, 20, verdict.Fail)
	warnGate := policy(50, 20, verdict.Warn)
	breach := func(n int, p config.Performance) Round { return round(n, 10, 200, p) }
	ok := func(n int, p config.Performance) Round { return round(n, 10, 11, p) }
	candErr := func(n int, p config.Performance) Round {
		r := Round{Round: n, Baseline: stats("baseline", 10), Candidate: stats("candidate", 10)}
		r.Candidate.TransportErrors = 2
		r.Candidate.ErrorKinds = map[string]int{"timeout": 2}
		r.Classify(p)
		return r
	}
	baseErr := func(n int, p config.Performance) Round {
		r := Round{Round: n, Baseline: stats("baseline", 10), Candidate: stats("candidate", 200)}
		r.Baseline.WarmupErrors = 1
		r.Classify(p)
		return r
	}
	tests := []struct {
		name   string
		rounds []Round
		p      config.Performance
		want   verdict.Outcome
		reason string
	}{
		{"clean", []Round{ok(1, failGate), ok(2, failGate)}, failGate, verdict.Pass, "within thresholds"},
		{"breach both rounds, fail gate", []Round{breach(1, failGate), breach(2, failGate)}, failGate, verdict.Fail, "repeated in all 2 rounds"},
		{"breach both rounds, warn gate", []Round{breach(1, warnGate), breach(2, warnGate)}, warnGate, verdict.Warn, "gate: warn"},
		{"breach one round only", []Round{breach(1, failGate), ok(2, failGate)}, failGate, verdict.Warn, "non-repeatable"},
		{"breach second round only", []Round{ok(1, failGate), breach(2, failGate)}, failGate, verdict.Warn, "non-repeatable"},
		{"candidate errors both rounds", []Round{candErr(1, failGate), candErr(2, failGate)}, failGate, verdict.Fail, "timeout=2"},
		{"candidate errors one round", []Round{candErr(1, failGate), ok(2, failGate)}, failGate, verdict.Warn, "non-repeatable"},
		{"errors then breach", []Round{candErr(1, failGate), breach(2, failGate)}, failGate, verdict.Fail, "repeated"},
		{"baseline errors", []Round{baseErr(1, failGate), breach(2, failGate)}, failGate, verdict.Inconclusive, "baseline had 1 error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reasons := Decide(tt.rounds, tt.p)
			if got != tt.want {
				t.Fatalf("Decide = %s, want %s (%v)", got, tt.want, reasons)
			}
			if !strings.Contains(strings.Join(reasons, " | "), tt.reason) {
				t.Fatalf("reasons %v do not mention %q", reasons, tt.reason)
			}
		})
	}
}

// --- measurement against real local servers ---

type tracker struct {
	mu       sync.Mutex
	inFlight map[string]int
	maxSeen  map[string]int
	overlap  bool
	hits     map[string]int
	order    []string
}

func newTracker() *tracker {
	return &tracker{inFlight: map[string]int{}, maxSeen: map[string]int{}, hits: map[string]int{}}
}

func (tr *tracker) server(t *testing.T, name string, delay time.Duration, status func(n int) int) *replay.Target {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.mu.Lock()
		tr.inFlight[name]++
		tr.hits[name]++
		if len(tr.order) == 0 || tr.order[len(tr.order)-1] != name {
			tr.order = append(tr.order, name)
		}
		if tr.inFlight[name] > tr.maxSeen[name] {
			tr.maxSeen[name] = tr.inFlight[name]
		}
		for other, c := range tr.inFlight {
			if other != name && c > 0 {
				tr.overlap = true
			}
		}
		tr.mu.Unlock()
		time.Sleep(delay)
		code := 200
		if status != nil {
			code = status(int(n.Add(1)))
		}
		w.WriteHeader(code)
		io.WriteString(w, `{"ok":true}`)
		tr.mu.Lock()
		tr.inFlight[name]--
		tr.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	u, err := config.ParseOrigin(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tg := replay.NewTarget(name, u, 8)
	t.Cleanup(tg.Close)
	return tg
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func req() replay.Request {
	return replay.Request{Method: "GET", Path: "/", Query: url.Values{}, Timeout: 2 * time.Second, MaxBytes: 1024}
}

func TestMeasureDetectsLargeLatencyIncrease(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 0, nil)
	cand := tr.server(t, "candidate", 40*time.Millisecond, nil)
	p := config.Performance{Warmup: 3, Samples: 100, Concurrency: 8, P95RelativePct: 50, P95AbsoluteMs: 20, Gate: verdict.Fail}

	res := Measure(context.Background(), base, cand, req(), p, 200, quiet)
	if res.Outcome != verdict.Fail {
		t.Fatalf("outcome = %s, reasons %v", res.Outcome, res.Reasons)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("rounds = %d", len(res.Rounds))
	}
	if got := res.Rounds[0].Order; got[0] != "baseline" || got[1] != "candidate" {
		t.Fatalf("round 1 order %v", got)
	}
	if got := res.Rounds[1].Order; got[0] != "candidate" || got[1] != "baseline" {
		t.Fatalf("round 2 order %v", got)
	}
	for _, r := range res.Rounds {
		for _, s := range []TargetStats{r.Baseline, r.Candidate} {
			if s.Attempts != 100 || s.Successes != 100 || s.WarmupAttempts != 3 || s.Errors() != 0 {
				t.Fatalf("round %d %s: %+v", r.Round, s.Target, s)
			}
		}
		if r.Status != RoundBreach {
			t.Fatalf("round %d status %s", r.Round, r.Status)
		}
	}
	// Read the server-side tracker under its lock (handlers ran on other goroutines).
	tr.mu.Lock()
	defer tr.mu.Unlock()
	// Server-observed request order: B, C (round 1), C, B (round 2).
	if strings.Join(tr.order, ",") != "baseline,candidate,baseline" {
		t.Fatalf("targets were not measured in alternating blocks: %v", tr.order)
	}
	if tr.overlap {
		t.Fatal("baseline and candidate were loaded simultaneously")
	}
	if tr.maxSeen["candidate"] > 8 || tr.maxSeen["baseline"] > 8 {
		t.Fatalf("concurrency exceeded: %v", tr.maxSeen)
	}
	if tr.hits["baseline"] != 206 || tr.hits["candidate"] != 206 {
		t.Fatalf("hits = %v, want 206 each (2 rounds x (3 warm-up + 100))", tr.hits)
	}
	if a := res.Aggregate; a.Baseline.Successes != 200 || a.Candidate.Attempts != 200 || a.Candidate.WarmupAttempts != 6 {
		t.Fatalf("aggregate = %+v", a)
	}
}

func TestMeasureSmallDifferenceDoesNotFail(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 1*time.Millisecond, nil)
	cand := tr.server(t, "candidate", 3*time.Millisecond, nil)
	p := config.Performance{Warmup: 2, Samples: 100, Concurrency: 8, P95RelativePct: 50, P95AbsoluteMs: 25, Gate: verdict.Fail}
	res := Measure(context.Background(), base, cand, req(), p, 200, quiet)
	if res.Outcome == verdict.Fail {
		t.Fatalf("a +2ms difference must not FAIL against a 25ms absolute threshold: %v", res.Reasons)
	}
}

func TestMeasureCandidateErrorsAreNotHidden(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 0, nil)
	cand := tr.server(t, "candidate", 0, func(n int) int {
		if n%10 == 0 {
			return 503
		}
		return 200
	})
	p := config.Performance{Warmup: 0, Samples: 100, Concurrency: 4, P95RelativePct: 50, P95AbsoluteMs: 20, Gate: verdict.Fail}
	res := Measure(context.Background(), base, cand, req(), p, 200, quiet)
	if res.Outcome != verdict.Fail {
		t.Fatalf("recurring candidate errors with fail gate: got %s %v", res.Outcome, res.Reasons)
	}
	for _, r := range res.Rounds {
		if r.Status != RoundCandidateErrors || r.Candidate.StatusErrors != 10 || r.Candidate.ErrorKinds["status_503"] != 10 || r.DeltaP95Ms != nil {
			t.Fatalf("round %d: %+v", r.Round, r)
		}
	}
}

func TestMeasureBaselineErrorsInconclusive(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 0, func(n int) int {
		if n == 1 {
			return 500 // first warm-up request fails
		}
		return 200
	})
	cand := tr.server(t, "candidate", 0, nil)
	p := config.Performance{Warmup: 2, Samples: 100, Concurrency: 4, P95RelativePct: 50, P95AbsoluteMs: 20, Gate: verdict.Fail}
	res := Measure(context.Background(), base, cand, req(), p, 200, quiet)
	if res.Outcome != verdict.Inconclusive {
		t.Fatalf("baseline warm-up error: got %s %v", res.Outcome, res.Reasons)
	}
	if res.Rounds[0].Baseline.ErrorKinds["warmup_status_500"] != 1 {
		t.Fatalf("warm-up error not recorded: %+v", res.Rounds[0].Baseline)
	}
}

func TestMeasureTimeoutsAreErrors(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 0, nil)
	cand := tr.server(t, "candidate", 60*time.Millisecond, nil)
	r := req()
	r.Timeout = 30 * time.Millisecond
	p := config.Performance{Warmup: 0, Samples: 100, Concurrency: 16, P95RelativePct: 50, P95AbsoluteMs: 20, Gate: verdict.Warn}
	res := Measure(context.Background(), base, cand, r, p, 200, quiet)
	if res.Outcome != verdict.Warn {
		t.Fatalf("timeouts in both rounds with warn gate: got %s %v", res.Outcome, res.Reasons)
	}
	if got := res.Rounds[0].Candidate; got.TransportErrors != 100 || got.ErrorKinds["timeout"] != 100 || got.P95Ms != nil {
		t.Fatalf("timeouts must be counted as errors, not samples: %+v", got)
	}
}

func TestMeasureCanceled(t *testing.T) {
	tr := newTracker()
	base := tr.server(t, "baseline", 5*time.Millisecond, nil)
	cand := tr.server(t, "candidate", 5*time.Millisecond, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := config.Performance{Warmup: 0, Samples: 1000, Concurrency: 1, P95RelativePct: 50, P95AbsoluteMs: 20, Gate: verdict.Fail}
	begin := time.Now()
	res := Measure(ctx, base, cand, req(), p, 200, quiet)
	if res.Outcome != verdict.Inconclusive || time.Since(begin) > 3*time.Second {
		t.Fatalf("canceled measurement: %s after %s", res.Outcome, time.Since(begin))
	}
}
