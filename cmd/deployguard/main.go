// Command deployguard runs the same API workload against a baseline and a
// candidate backend and reports behavioral, contract, error and performance
// regressions with CI-friendly exit codes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/engine"
	"github.com/AddysEdge/deployguard/internal/report"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal, restore default handling so a second
		// Ctrl+C terminates immediately.
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `DeployGuard compares a candidate backend release against the current baseline
by replaying the same GET/HEAD workload against both, and reports behavioral,
contract, error and performance regressions before deployment.

Usage:
  deployguard compare  --config FILE --baseline URL --candidate URL [--report FILE]
  deployguard validate --config FILE
  deployguard version
  deployguard help [command]

Commands:
  compare   Run the workload against both targets and decide PASS/WARN/FAIL/INCONCLUSIVE
  validate  Check a workload file (schema, limits, environment references) without sending requests
  version   Print the version
  help      Show help for a command

Exit codes:
  0  PASS or WARN (WARN is non-blocking)
  1  FAIL: a regression was detected
  2  INCONCLUSIVE: the result cannot safely approve a release
  3  invalid usage or config, report-write failure, or internal error
  130 canceled by the user (SIGINT/SIGTERM); a partial report is still written

Only target controlled environments (staging, fixtures). GET and HEAD are the
only methods, but a poorly designed API can still have side effects on GET.
`

const compareUsage = `Usage:
  deployguard compare --config FILE --baseline URL --candidate URL [flags]

Replays every scenario in the workload against the baseline and the candidate,
compares status, media type, redirect Location and body semantically, optionally
measures p95 latency, and exits with the release verdict.

Required:
  --config FILE      versioned YAML workload (see README "Workload configuration")
  --baseline URL     origin of the current release, e.g. http://127.0.0.1:8080
  --candidate URL    origin of the candidate release, e.g. http://127.0.0.1:8081
                     URLs must be http(s) origins: no path, query, fragment or credentials.

Optional:
  --report FILE      write the versioned JSON report here (directories are created).
                     It is written before DeployGuard exits, including on FAIL,
                     INCONCLUSIVE and cancellation.
  --log-level LEVEL  debug, info, warn or error (default info). Logs go to stderr.
  --log-format FMT   text or json (default text)

Example:
  deployguard compare --config examples/deployguard.yaml \
    --baseline http://127.0.0.1:8080 --candidate http://127.0.0.1:8081 \
    --report reports/run.json
`

const validateUsage = `Usage:
  deployguard validate --config FILE

Decodes the workload strictly, validates every field and limit, and resolves
${ENV} references in headers (values are never printed). Sends no requests.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return verdict.ExitError
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) > 1 {
			switch args[1] {
			case "compare":
				fmt.Fprint(stdout, compareUsage)
				return 0
			case "validate":
				fmt.Fprint(stdout, validateUsage)
				return 0
			}
		}
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "deployguard %s\n", version)
		return 0
	case "compare":
		return runCompare(ctx, args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "deployguard: unknown command %q\n\n%s", args[0], usage)
		return verdict.ExitError
	}
}

func newFlagSet(name, help string, stdout, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stdout, help) }
	return fs
}

// parseFlags returns (done, exitCode). done is true when the caller should
// return immediately (help was printed or the flags were invalid).
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (bool, int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		fmt.Fprintf(stderr, "Run 'deployguard help %s' for usage.\n", fs.Name())
		return true, verdict.ExitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "deployguard %s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return true, verdict.ExitError
	}
	return false, 0
}

func runCompare(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("compare", compareUsage, stdout, stderr)
	cfgPath := fs.String("config", "", "workload file")
	baselineRaw := fs.String("baseline", "", "baseline origin URL")
	candidateRaw := fs.String("candidate", "", "candidate origin URL")
	reportPath := fs.String("report", "", "JSON report path")
	logLevel := fs.String("log-level", "info", "log level")
	logFormat := fs.String("log-format", "text", "log format")
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}

	var problems []string
	if *cfgPath == "" {
		problems = append(problems, "--config is required")
	}
	baseline, err := requireOrigin("--baseline", *baselineRaw)
	if err != nil {
		problems = append(problems, err.Error())
	}
	candidate, err := requireOrigin("--candidate", *candidateRaw)
	if err != nil {
		problems = append(problems, err.Error())
	}
	logger, err := newLogger(stderr, *logLevel, *logFormat)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "deployguard compare: %s\nRun 'deployguard help compare' for usage.\n", strings.Join(problems, "; "))
		return verdict.ExitError
	}

	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "deployguard compare: %v\n", err)
		return verdict.ExitError
	}
	if baseline.String() == candidate.String() {
		logger.Warn("baseline and candidate are the same origin; differences between releases cannot be detected")
	}

	runID := engine.NewRunID()
	logger.Info("run started", "run_id", runID, "config", *cfgPath, "scenarios", len(cfg.Scenarios),
		"planned_requests", cfg.PlannedRequests(), "baseline", baseline.String(), "candidate", candidate.String())
	rep := engine.Run(ctx, cfg, baseline, candidate, engine.Options{
		ConfigPath: *cfgPath, Version: version, RunID: runID, Logger: logger,
	})

	report.Render(stdout, rep)
	if *reportPath != "" {
		if err := report.Write(*reportPath, rep); err != nil {
			fmt.Fprintf(stderr, "\nERROR: the JSON report could not be written to %s: %v\n"+
				"ERROR: treating this run as a DeployGuard error (exit %d); the verdict above was %s.\n",
				*reportPath, err, verdict.ExitError, rep.Outcome)
			return verdict.ExitError
		}
		fmt.Fprintf(stdout, "Report: %s\n", *reportPath)
	}
	return rep.ExitCode
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("validate", validateUsage, stdout, stderr)
	cfgPath := fs.String("config", "", "workload file")
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}
	if *cfgPath == "" {
		fmt.Fprintln(stderr, "deployguard validate: --config is required")
		return verdict.ExitError
	}
	cfg, err := config.Load(*cfgPath, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "deployguard validate: %v\n", err)
		return verdict.ExitError
	}
	var gated []string
	for _, sc := range cfg.Scenarios {
		if sc.Performance != nil {
			gated = append(gated, sc.Name)
		}
	}
	perfNote := "none"
	if len(gated) > 0 {
		perfNote = strings.Join(gated, ", ")
	}
	fmt.Fprintf(stdout, "%s: valid (version %d, %d scenario(s), %d planned request(s); performance gates: %s)\n",
		*cfgPath, cfg.Version, len(cfg.Scenarios), cfg.PlannedRequests(), perfNote)
	return 0
}

func requireOrigin(flagName, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required", flagName)
	}
	u, err := config.ParseOrigin(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", flagName, err)
	}
	return u, nil
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level must be debug, info, warn or error")
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("--log-format must be text or json")
}
