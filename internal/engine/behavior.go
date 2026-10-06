package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/perf"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/report"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// observe takes the functional observations of one scenario in interleaved
// order: baseline, candidate, baseline, candidate.
func observe(ctx context.Context, base, cand *replay.Target, sc config.Scenario, maxBytes int64) report.Behavior {
	req := replay.RequestFor(sc, maxBytes)
	obs := make([]replay.Response, 0, 2*config.FunctionalObservations)
	for k := 0; k < config.FunctionalObservations; k++ {
		obs = append(obs, base.Do(ctx, req, true), cand.Do(ctx, req, true))
	}
	baseURL, _ := replay.BuildURL(base.Origin, req.Path, req.Query)
	candURL, _ := replay.BuildURL(cand.Origin, req.Path, req.Query)
	return Evaluate(sc, baseURL, candURL, obs)
}

// Evaluate judges a scenario's functional observations, given in the order
// baseline, candidate, baseline, candidate. It is pure so that every verdict
// rule can be tested without a network.
//
// Rules, in order:
//  1. A canceled observation makes the scenario INCONCLUSIVE.
//  2. A baseline transport failure, oversized body, 5xx, malformed declared
//     JSON, or expect_status violation means there is no trustworthy
//     reference: INCONCLUSIVE.
//  3. Baseline observations that differ (after ignore rules): INCONCLUSIVE.
//  4. The same problems on the candidate against a healthy baseline: FAIL.
//  5. Candidate observations that differ while the baseline is stable: FAIL.
//  6. Otherwise the first baseline and candidate observations are compared.
func Evaluate(sc config.Scenario, baseURL, candURL *url.URL, obs []replay.Response) report.Behavior {
	beh := report.Behavior{
		ExpectStatus:    sc.ExpectStatus,
		StrictAdditions: sc.StrictAdditions,
		Findings:        []compare.Finding{},
		IgnoreRules:     []report.IgnoreRule{},
	}
	var (
		baseObs, candObs           []compare.Observed
		baseProblems, candProblems []string
		canceled                   bool
	)
	for i, r := range obs {
		isBase := i%2 == 0
		target, u := "candidate", candURL
		if isBase {
			target, u = "baseline", baseURL
		}
		o, problem := check(sc, r, u)
		beh.Observations = append(beh.Observations, report.Observation{
			Sequence: i + 1, Target: target, Status: r.Status, MediaType: o.MediaType,
			Location: o.Location.Display, BodyBytes: r.BodyBytes, DurationMs: perf.Ms(r.Duration), Error: r.Failure,
		})
		if r.Failure != nil && r.Failure.Kind == replay.FailCanceled {
			canceled = true
		}
		if isBase {
			baseObs = append(baseObs, o)
			baseProblems = appendUnique(baseProblems, problem)
		} else {
			candObs = append(candObs, o)
			candProblems = appendUnique(candProblems, problem)
		}
	}

	switch {
	case canceled:
		beh.Outcome = verdict.Inconclusive
		beh.Reasons = []string{"run canceled before all observations completed"}
		beh.IgnoreRules = ignoreReport(sc, nil, false, nil, nil)
		return beh
	case len(baseProblems) > 0:
		beh.Outcome = verdict.Inconclusive
		for _, p := range baseProblems {
			beh.Reasons = append(beh.Reasons, "baseline is not a trustworthy reference: "+p)
		}
		for _, p := range candProblems {
			beh.Reasons = append(beh.Reasons, "candidate (not judged without a healthy baseline): "+p)
		}
		beh.IgnoreRules = ignoreReport(sc, nil, false, nil, nil)
		return beh
	}

	usage := map[string]int{}
	stability := compare.Options{Ignore: sc.Ignore, StrictAdditions: true}
	b1, b2 := baseObs[0], baseObs[1]
	bs := compare.Compare(b1, b2, stability)
	merge(usage, bs.Suppressed)
	if len(bs.Findings) > 0 {
		beh.Outcome = verdict.Inconclusive
		beh.Reasons = []string{fmt.Sprintf("baseline unstable: its observations differ in %d place(s) after ignore rules, so it cannot define the expected behavior", len(bs.Findings)+bs.Omitted)}
		beh.BaselineInstability = bs.Findings
		beh.IgnoreRules = ignoreReport(sc, usage, true, b1.JSON, bs.Findings)
		return beh
	}

	if len(candProblems) > 0 {
		beh.Outcome = verdict.Fail
		for _, p := range candProblems {
			beh.Reasons = append(beh.Reasons, "candidate failed against a healthy baseline: "+p)
		}
		beh.IgnoreRules = ignoreReport(sc, usage, true, b1.JSON, nil)
		return beh
	}

	c1, c2 := candObs[0], candObs[1]
	cs := compare.Compare(c1, c2, stability)
	merge(usage, cs.Suppressed)
	x := compare.Compare(b1, c1, compare.Options{Ignore: sc.Ignore, StrictAdditions: sc.StrictAdditions})
	merge(usage, x.Suppressed)

	beh.Findings, beh.FindingsOmitted = x.Findings, x.Omitted
	beh.Outcome = x.Outcome()
	if n := len(x.Findings) + x.Omitted; n > 0 {
		fails, warns := 0, 0
		for _, f := range x.Findings {
			if f.Severity == verdict.Fail {
				fails++
			} else {
				warns++
			}
		}
		beh.Reasons = append(beh.Reasons, fmt.Sprintf("candidate differs from baseline: %d FAIL and %d WARN finding(s)", fails+x.Omitted, warns))
	}
	if len(cs.Findings) > 0 {
		beh.Outcome = verdict.Fail
		beh.CandidateInstability = cs.Findings
		beh.Reasons = append(beh.Reasons, fmt.Sprintf("candidate unstable: its observations differ in %d place(s) while the baseline was stable", len(cs.Findings)+cs.Omitted))
	}
	if beh.Outcome == verdict.Pass {
		what := fmt.Sprintf("status %d", b1.Status)
		if b1.MediaType != "" {
			what += ", " + b1.MediaType
		}
		if sc.Method == http.MethodHead {
			what += "; HEAD, body not compared"
		}
		beh.Reasons = []string{"candidate matched baseline (" + what + ")"}
	}
	all := append(append(append([]compare.Finding{}, x.Findings...), cs.Findings...), bs.Findings...)
	beh.IgnoreRules = ignoreReport(sc, usage, true, b1.JSON, all)
	return beh
}

// check normalizes one response and returns a health problem, if any.
func check(sc config.Scenario, r replay.Response, u *url.URL) (compare.Observed, string) {
	if r.Failure != nil {
		return compare.Observed{}, fmt.Sprintf("%s: %s", r.Failure.Kind, r.Failure.Message)
	}
	o, err := compare.Observe(sc.Method, r.Status, r.ContentType, r.Location, r.HasLocation, r.Body, u)
	switch {
	case r.Status >= 500:
		return o, fmt.Sprintf("server error status %d", r.Status)
	case sc.ExpectStatus != 0 && r.Status != sc.ExpectStatus:
		return o, fmt.Sprintf("status %d does not match expect_status %d", r.Status, sc.ExpectStatus)
	case err != nil:
		return o, err.Error()
	}
	return o, ""
}

func appendUnique(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

func merge(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

// ignoreReport explains, per configured ignore rule, whether it suppressed
// anything and if not, why.
func ignoreReport(sc config.Scenario, usage map[string]int, evaluated bool, baseDoc *compare.Value, findings []compare.Finding) []report.IgnoreRule {
	out := []report.IgnoreRule{}
	for _, p := range sc.Ignore {
		r := report.IgnoreRule{Path: p, Suppressed: usage[p], Status: "applied"}
		if r.Suppressed == 0 {
			r.Status = "unused"
			r.Note = unusedReason(p, evaluated, baseDoc, findings)
		}
		out = append(out, r)
	}
	return out
}

func unusedReason(p string, evaluated bool, baseDoc *compare.Value, findings []compare.Finding) string {
	if !evaluated {
		return "not evaluated: baseline was not a trustworthy reference"
	}
	if baseDoc == nil {
		return "baseline response was not a JSON document"
	}
	v, ok := compare.Lookup(baseDoc, p)
	if !ok {
		return "path not present in the baseline response"
	}
	switch v.Kind {
	case compare.Object, compare.Array, compare.Null:
		return fmt.Sprintf("path holds %s; only string, number and boolean values can be ignored", v.Kind)
	}
	for _, f := range findings {
		if f.Path == p {
			return "a structural or type change at this path cannot be ignored"
		}
	}
	return "value did not differ in any comparison"
}
