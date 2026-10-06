package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/perf"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// maxFindingsShown limits findings printed per scenario; the JSON report has all.
const maxFindingsShown = 8

// Render writes the human-readable summary. It uses plain ASCII and no color
// so it reads the same in CI logs and Windows consoles.
func Render(w io.Writer, r *Report) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("DeployGuard %s  run %s", r.Tool.Version, r.Run.ID)
	p("  baseline   %s", r.Run.Baseline)
	p("  candidate  %s", r.Run.Candidate)
	p("  workload   %s: %d scenario(s), %d request(s) planned, %d issued, %.1fs",
		r.Run.ConfigPath, len(r.Scenarios), r.Run.PlannedRequests, r.Run.IssuedRequests, r.Run.DurationMs/1000)
	p("")

	nameWidth := 8
	for _, s := range r.Scenarios {
		nameWidth = max(nameWidth, len(s.Name))
	}
	for _, s := range r.Scenarios {
		p("%-12s %-*s  %s %s", s.Outcome, nameWidth, s.Name, s.Method, s.Path)
		renderBehavior(p, s.Behavior)
		renderPerformance(p, s.Performance)
	}

	p("")
	if r.Run.Canceled {
		p("Run canceled before completion; the result cannot approve a release.")
	}
	p("Result: %s (exit code %d)", r.Outcome, r.ExitCode)
	p("  %d FAIL, %d INCONCLUSIVE, %d WARN, %d PASS", r.Summary.Fail, r.Summary.Inconclusive, r.Summary.Warn, r.Summary.Pass)
	for _, reason := range r.Reasons {
		p("  - %s", reason)
	}
}

func renderBehavior(p func(string, ...any), b Behavior) {
	first := ""
	if len(b.Reasons) > 0 {
		first = b.Reasons[0]
	}
	p("  behavior     %-12s %s", b.Outcome, first)
	for _, reason := range b.Reasons[min(1, len(b.Reasons)):] {
		p("                            %s", reason)
	}
	printFindings(p, "", b.Findings, b.FindingsOmitted)
	printFindings(p, "baseline instability: ", b.BaselineInstability, 0)
	printFindings(p, "candidate instability: ", b.CandidateInstability, 0)
	for _, ig := range b.IgnoreRules {
		if ig.Status == "applied" {
			p("               ignore %s: applied, suppressed %d difference(s)", ig.Path, ig.Suppressed)
		} else {
			p("               ignore %s: UNUSED (%s)", ig.Path, ig.Note)
		}
	}
}

func printFindings(p func(string, ...any), prefix string, fs []compare.Finding, omitted int) {
	for i, f := range fs {
		if i == maxFindingsShown {
			omitted += len(fs) - i
			break
		}
		where := ""
		if f.IsBody() {
			where = f.Path
			if where == "" {
				where = "(document root)"
			}
		}
		p("               %s%-4s %-20s %s %s -> %s", prefix, f.Severity, f.Category, where, f.Baseline, f.Candidate)
	}
	if omitted > 0 {
		p("               ... %d more finding(s) in the JSON report", omitted)
	}
}

func renderPerformance(p func(string, ...any), r perf.Result) {
	first := ""
	if len(r.Reasons) > 0 {
		first = r.Reasons[0]
	}
	p("  performance  %-12s %s", r.Outcome, first)
	for _, reason := range r.Reasons[min(1, len(r.Reasons)):] {
		p("                            %s", reason)
	}
	for _, rd := range r.Rounds {
		p("               round %d %-20s p95 %s -> %s  delta %s  n=%d/%d ok, errors %d/%d  [%s]",
			rd.Round, strings.Join(rd.Order, "->"),
			fmtMs(rd.Baseline.P95Ms), fmtMs(rd.Candidate.P95Ms), fmtDelta(rd.DeltaP95Ms, rd.DeltaP95Pct),
			rd.Baseline.Successes, rd.Candidate.Successes, rd.Baseline.Errors(), rd.Candidate.Errors(), rd.Status)
	}
}

func fmtMs(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1fms", *v)
}

func fmtDelta(ms, pct *float64) string {
	if ms == nil {
		return "n/a"
	}
	if pct == nil {
		return fmt.Sprintf("%+.1fms", *ms)
	}
	return fmt.Sprintf("%+.1fms (%+.1f%%)", *ms, *pct)
}

// Reason builds the one-line explanation of a non-PASS scenario used in the
// final verdict section.
func Reason(s Scenario) string {
	var parts []string
	if s.Behavior.Outcome != verdict.Pass {
		detail := ""
		if len(s.Behavior.Findings) > 0 {
			var cats []string
			for i, f := range s.Behavior.Findings {
				if i == 3 {
					cats = append(cats, "...")
					break
				}
				if f.IsBody() {
					cats = append(cats, string(f.Category)+" "+f.Path)
				} else {
					cats = append(cats, fmt.Sprintf("%s %s->%s", f.Category, f.Baseline, f.Candidate))
				}
			}
			detail = strings.Join(cats, ", ")
		} else if len(s.Behavior.Reasons) > 0 {
			detail = s.Behavior.Reasons[0]
		}
		parts = append(parts, fmt.Sprintf("behavior %s: %s", s.Behavior.Outcome, detail))
	}
	if s.Performance.Outcome != verdict.Pass && s.Performance.Outcome != verdict.Skipped {
		detail := ""
		if len(s.Performance.Reasons) > 0 {
			detail = s.Performance.Reasons[0]
		}
		parts = append(parts, fmt.Sprintf("performance %s: %s", s.Performance.Outcome, detail))
	}
	return fmt.Sprintf("%s %s: %s", s.Name, s.Outcome, strings.Join(parts, "; "))
}
