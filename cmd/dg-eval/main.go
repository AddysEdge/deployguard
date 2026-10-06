// Command dg-eval runs DeployGuard's seeded regression suite against real
// local fixture servers, compares observed outcomes with ground truth, and
// writes measured results.
//
//	go run ./cmd/dg-eval --out reports/eval.json
//
// Exit status: 0 when every case matches, 1 on any mismatch, 3 on error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/AddysEdge/deployguard/internal/eval"
)

func main() {
	root := flag.String("root", ".", "repository root (where examples/ lives)")
	out := flag.String("out", "reports/eval.json", "where to write the JSON results")
	only := flag.String("cases", "", "comma-separated case IDs to run (default: all)")
	verbose := flag.Bool("v", false, "log engine progress to stderr")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cases := eval.Cases
	if *only != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(*only, ",") {
			want[strings.TrimSpace(id)] = true
		}
		cases = nil
		for _, c := range eval.Cases {
			if want[c.ID] {
				cases = append(cases, c)
			}
		}
		if len(cases) == 0 {
			fmt.Fprintln(os.Stderr, "dg-eval: no matching cases")
			os.Exit(3)
		}
	}

	logOut := io.Discard
	if *verbose {
		logOut = os.Stderr
	}
	res, err := eval.Run(ctx, *root, cases, slog.New(slog.NewTextHandler(logOut, nil)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dg-eval:", err)
		os.Exit(3)
	}

	printTable(os.Stdout, res)

	data, _ := json.MarshalIndent(res, "", "  ")
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err == nil {
		err = os.WriteFile(*out, append(data, '\n'), 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dg-eval: could not write %s: %v\n", *out, err)
		os.Exit(3)
	}
	fmt.Printf("\nResults written to %s\n", *out)
	if !res.AllMatched() {
		os.Exit(1)
	}
}

func printTable(w io.Writer, res *eval.Result) {
	e := res.Environment
	fmt.Fprintf(w, "DeployGuard seeded evaluation  %s  %s %s/%s  %d CPU(s)\n", e.Date.Format("2006-01-02 15:04 MST"), e.GoVersion, e.OS, e.Arch, e.CPUs)
	fmt.Fprintf(w, "fixture: R5 slow delay %s, N1 noise up to %s\n\n", e.SlowDelay, e.NoiseMax)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCASE\tKIND\tEXPECTED\tOBSERVED\tMATCH\tCLASS\tTIME")
	for _, c := range res.Cases {
		match := "yes"
		if !c.Match {
			match = "NO"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.0fms\n", c.ID, c.Title, c.Kind, c.Expected, c.Observed, match, c.Classification, c.DurationMs)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nMeasured p95 (nearest-rank, successful measured attempts per round):")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSCENARIO\tROUND\tBASELINE p95\tCANDIDATE p95\tDELTA\tN (B/C)\tSTATUS")
	for _, c := range res.Cases {
		for _, m := range c.Performance {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%d/%d\t%s\n", c.ID, m.Scenario, m.Round, ms(m.BaselineP95Ms), ms(m.CandidateP95Ms), ms(m.DeltaP95Ms), m.BaselineN, m.CandidateN, m.Status)
		}
	}
	tw.Flush()

	fmt.Fprintf(w, "\nMatched %d/%d cases.\n", res.Summary.Matched, res.Summary.Cases)
	for _, kind := range []string{"behavior", "performance", "workload"} {
		k, ok := res.Summary.ByKind[kind]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "  %-11s %d/%d matched; regressions detected %d/%d (FN %d); false positives %d/%d\n",
			kind, k.Matched, k.Cases, k.TruePositives, k.TruePositives+k.FalseNegatives, k.FalseNegatives,
			k.FalsePositives, k.TrueNegatives+k.FalsePositives)
	}
	fmt.Fprintln(w, "\n"+res.Note)
}

func ms(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2fms", *v)
}
