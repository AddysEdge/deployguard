// Package report defines DeployGuard's versioned JSON report (schema_version
// 1), writes it atomically, and renders the concise terminal summary.
//
// The report deliberately contains no request headers, resolved environment
// variables, or response bodies.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AddysEdge/deployguard/internal/compare"
	"github.com/AddysEdge/deployguard/internal/perf"
	"github.com/AddysEdge/deployguard/internal/replay"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// SchemaVersion is incremented on any incompatible report change.
const SchemaVersion = 1

// Redacted replaces values that must not appear in reports.
const Redacted = "<redacted>"

// Report is the top-level JSON document.
type Report struct {
	SchemaVersion int             `json:"schema_version"`
	Tool          Tool            `json:"tool"`
	Run           Run             `json:"run"`
	Settings      Settings        `json:"settings"`
	Outcome       verdict.Outcome `json:"outcome"`
	ExitCode      int             `json:"exit_code"`
	Summary       Summary         `json:"summary"`
	Reasons       []string        `json:"reasons"`
	Scenarios     []Scenario      `json:"scenarios"`
}

// Tool identifies the producer.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Run is run metadata.
type Run struct {
	ID              string    `json:"id"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	DurationMs      float64   `json:"duration_ms"`
	Canceled        bool      `json:"canceled"`
	ConfigPath      string    `json:"config_path"`
	ConfigVersion   int       `json:"config_version"`
	Baseline        string    `json:"baseline"`
	Candidate       string    `json:"candidate"`
	PlannedRequests int       `json:"planned_requests"`
	IssuedRequests  int64     `json:"issued_requests"` // logical exchanges, not guaranteed wire-level attempts
}

// Settings are the effective, non-secret global settings.
type Settings struct {
	TimeoutMs        int64 `json:"timeout_ms"`
	MaxResponseBytes int64 `json:"max_response_bytes"`
	Concurrency      int   `json:"concurrency"`
	MaxTotalRequests int   `json:"max_total_requests"`
}

// Summary counts scenario outcomes.
type Summary struct {
	Total        int `json:"total"`
	Pass         int `json:"pass"`
	Warn         int `json:"warn"`
	Fail         int `json:"fail"`
	Inconclusive int `json:"inconclusive"`
}

// Add counts one scenario outcome.
func (s *Summary) Add(o verdict.Outcome) {
	s.Total++
	switch o {
	case verdict.Pass:
		s.Pass++
	case verdict.Warn:
		s.Warn++
	case verdict.Fail:
		s.Fail++
	case verdict.Inconclusive:
		s.Inconclusive++
	}
}

// Scenario is one scenario's result.
type Scenario struct {
	Name        string              `json:"name"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       map[string][]string `json:"query,omitempty"` // parameter names with every value replaced by "<redacted>"
	Outcome     verdict.Outcome     `json:"outcome"`
	Behavior    Behavior            `json:"behavior"`
	Performance perf.Result         `json:"performance"`
}

// Behavior is the functional/contract sub-result.
type Behavior struct {
	Outcome         verdict.Outcome `json:"outcome"`
	Reasons         []string        `json:"reasons"`
	ExpectStatus    int             `json:"expect_status,omitempty"`
	StrictAdditions bool            `json:"strict_additions"`
	Observations    []Observation   `json:"observations"`
	// Findings are the RETAINED baseline/candidate findings: at most 100,
	// FAIL findings retained in preference to WARN, sorted deterministically.
	Findings        []compare.Finding `json:"findings"`
	FindingsOmitted int               `json:"findings_omitted"` // differences found but not retained
	// FindingCounts counts every discovered difference by severity, retained
	// or omitted. The behavioral verdict is derived from these counts.
	FindingCounts               compare.Counts    `json:"finding_counts"`
	BaselineInstability         []compare.Finding `json:"baseline_instability,omitempty"`
	BaselineInstabilityOmitted  int               `json:"baseline_instability_omitted,omitempty"`
	CandidateInstability        []compare.Finding `json:"candidate_instability,omitempty"`
	CandidateInstabilityOmitted int               `json:"candidate_instability_omitted,omitempty"`
	IgnoreRules                 []IgnoreRule      `json:"ignore_rules"`
}

// Observation is one functional request. Bodies and headers are omitted.
type Observation struct {
	Sequence   int             `json:"sequence"`
	Target     string          `json:"target"`
	Status     int             `json:"status,omitempty"`
	MediaType  string          `json:"media_type,omitempty"`
	Location   string          `json:"location,omitempty"` // redacted display form
	BodyBytes  int64           `json:"body_bytes"`
	DurationMs float64         `json:"duration_ms"`
	Error      *replay.Failure `json:"error,omitempty"`
}

// IgnoreRule reports whether a configured ignore pointer actually suppressed
// anything, so configuration never silently pretends to suppress behavior.
type IgnoreRule struct {
	Path       string `json:"path"`
	Status     string `json:"status"` // "applied" or "unused"
	Suppressed int    `json:"suppressed"`
	Note       string `json:"note,omitempty"`
}

// Write stores the report as indented JSON. It writes to a temporary file in
// the destination directory and renames it, so readers never see a partial
// report.
func Write(path string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".deployguard-report-*.tmp")
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("move report into place: %w", err)
	}
	return nil
}
