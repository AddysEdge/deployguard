// Package config loads and validates DeployGuard's versioned YAML workload.
//
// Decoding is strict (unknown fields, duplicate keys, extra documents and
// wrongly typed values are rejected) and is followed by explicit validation
// that produces field-specific error messages. Header values may reference
// environment variables as ${NAME}; resolved values are never included in
// errors or in the String form of a Header.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/AddysEdge/deployguard/internal/verdict"
	"go.yaml.in/yaml/v3"
)

// SupportedVersion is the only workload schema version this build accepts.
const SupportedVersion = 1

// Defaults and hard limits. These values are documented in the README; keep
// them in sync.
const (
	DefaultTimeout = 5 * time.Second
	MaxTimeout     = 60 * time.Second

	DefaultMaxResponseBytes int64 = 1 << 20  // 1 MiB
	MaxMaxResponseBytes     int64 = 16 << 20 // 16 MiB

	DefaultConcurrency = 4
	MaxConcurrency     = 16

	DefaultMaxTotalRequests = 5000
	MaxMaxTotalRequests     = 100000

	MaxScenarios       = 200
	MaxConfigFileBytes = 1 << 20

	// FunctionalObservations is the number of functional observations taken
	// of each target per scenario (interleaved baseline/candidate).
	FunctionalObservations = 2

	// MeasuredRounds is fixed: round 1 runs baseline then candidate, round 2
	// runs candidate then baseline.
	MeasuredRounds = 2

	DefaultWarmup = 10
	MaxWarmup     = 1000
	MinSamples    = 100
	MaxSamples    = 5000

	MaxRelativePct = 1000.0
	MaxAbsoluteMs  = 60000.0
)

// Config is a validated workload.
type Config struct {
	Version   int
	Settings  Settings
	Scenarios []Scenario
}

// Settings are global request limits.
type Settings struct {
	Timeout          time.Duration
	MaxResponseBytes int64
	Concurrency      int
	MaxTotalRequests int
}

// Scenario is one named request shape replayed against both targets.
type Scenario struct {
	Name            string
	Method          string // GET or HEAD
	Path            string // root-relative, validated
	Query           url.Values
	Headers         []Header // sorted by canonical name
	Timeout         time.Duration
	ExpectStatus    int      // 0 means "not configured"
	Ignore          []string // RFC 6901 JSON Pointers, in config order
	StrictAdditions bool
	Performance     *Performance // nil when performance is not measured
}

// Performance is the opt-in latency gate policy for a scenario.
type Performance struct {
	Warmup         int     // requests per target block, excluded from metrics
	Samples        int     // measured requests per target per round
	Concurrency    int     // in-flight requests while measuring one target
	P95RelativePct float64 // candidate p95 must exceed baseline p95 by more than this percentage...
	P95AbsoluteMs  float64 // ...and by more than this many milliseconds to count as a breach
	Gate           verdict.Outcome
}

// PlannedRequests returns the number of HTTP requests a full run of this
// scenario issues across both targets.
func (s Scenario) PlannedRequests() int {
	n := 2 * FunctionalObservations
	if p := s.Performance; p != nil {
		n += MeasuredRounds * 2 * (p.Warmup + p.Samples)
	}
	return n
}

// PlannedRequests returns the total requests a full run issues.
func (c *Config) PlannedRequests() int {
	total := 0
	for _, s := range c.Scenarios {
		total += s.PlannedRequests()
	}
	return total
}

// LookupFunc resolves an environment variable, like os.LookupEnv.
type LookupFunc func(name string) (string, bool)

// Load reads, decodes and validates the workload file at path.
func Load(path string, lookup LookupFunc) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) > MaxConfigFileBytes {
		return nil, fmt.Errorf("config file exceeds %d bytes", MaxConfigFileBytes)
	}
	return Parse(data, lookup)
}

// Parse decodes and validates a workload document.
func Parse(data []byte, lookup LookupFunc) (*Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	raw, err := decodeStrict(data)
	if err != nil {
		return nil, err
	}
	return validate(raw, lookup)
}

// ValidationError lists every problem found after decoding.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

var goTypeName = regexp.MustCompile(` in type config\.\w+`)

func decodeStrict(data []byte) (*rawFile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawFile
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("invalid config: document is empty")
		}
		return nil, fmt.Errorf("invalid config: %s", goTypeName.ReplaceAllString(err.Error(), ""))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid config: expected exactly one YAML document, found more")
	}
	return &raw, nil
}

// Duration decodes YAML strings such as "5s" or "250ms".
type Duration time.Duration

// UnmarshalYAML accepts only string scalars parseable by time.ParseDuration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: duration must be a string such as \"5s\" or \"250ms\"", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

type rawFile struct {
	Version   *int          `yaml:"version"`
	Settings  *rawSettings  `yaml:"settings"`
	Scenarios []rawScenario `yaml:"scenarios"`
}

type rawSettings struct {
	Timeout          *Duration `yaml:"timeout"`
	MaxResponseBytes *int64    `yaml:"max_response_bytes"`
	Concurrency      *int      `yaml:"concurrency"`
	MaxTotalRequests *int      `yaml:"max_total_requests"`
}

type rawScenario struct {
	Name            string            `yaml:"name"`
	Method          string            `yaml:"method"`
	Path            string            `yaml:"path"`
	Query           map[string]string `yaml:"query"`
	Headers         map[string]string `yaml:"headers"`
	Timeout         *Duration         `yaml:"timeout"`
	ExpectStatus    *int              `yaml:"expect_status"`
	Ignore          []string          `yaml:"ignore"`
	StrictAdditions *bool             `yaml:"strict_additions"`
	Performance     *rawPerformance   `yaml:"performance"`
}

type rawPerformance struct {
	Warmup         *int     `yaml:"warmup"`
	Samples        *int     `yaml:"samples"`
	Concurrency    *int     `yaml:"concurrency"`
	P95RelativePct *float64 `yaml:"p95_relative_pct"`
	P95AbsoluteMs  *float64 `yaml:"p95_absolute_ms"`
	Gate           *string  `yaml:"gate"`
}
