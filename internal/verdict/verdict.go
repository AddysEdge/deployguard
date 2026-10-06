// Package verdict defines DeployGuard outcomes, their precedence, and the
// mapping from a final outcome to a process exit code.
package verdict

// Outcome is the result of a check, a scenario, or a whole run.
type Outcome string

const (
	Pass         Outcome = "PASS"
	Warn         Outcome = "WARN"
	Fail         Outcome = "FAIL"
	Inconclusive Outcome = "INCONCLUSIVE"
	// Skipped marks a sub-result (for example performance) that produced no
	// evidence. It never participates in precedence.
	Skipped Outcome = "SKIPPED"
)

// Exit codes are part of DeployGuard's CI contract.
const (
	ExitPass         = 0 // PASS or WARN
	ExitFail         = 1
	ExitInconclusive = 2
	ExitError        = 3 // invalid config, usage error, report-write failure, internal error
	ExitCanceled     = 130
)

// rank orders outcomes by precedence: FAIL > INCONCLUSIVE > WARN > PASS.
func rank(o Outcome) int {
	switch o {
	case Fail:
		return 4
	case Inconclusive:
		return 3
	case Warn:
		return 2
	case Pass:
		return 1
	default:
		return 0
	}
}

// Worst returns the highest-precedence outcome. SKIPPED (and unknown values)
// are ignored; if nothing remains the result is PASS.
func Worst(outcomes ...Outcome) Outcome {
	worst := Pass
	for _, o := range outcomes {
		if rank(o) > rank(worst) {
			worst = o
		}
	}
	return worst
}

// ExitCode maps a final run outcome to the documented exit code.
func ExitCode(o Outcome) int {
	switch o {
	case Pass, Warn:
		return ExitPass
	case Fail:
		return ExitFail
	case Inconclusive:
		return ExitInconclusive
	default:
		return ExitError
	}
}
