package compare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/AddysEdge/deployguard/internal/config"
	"github.com/AddysEdge/deployguard/internal/verdict"
)

// Category names a kind of difference. Values are part of the report schema.
type Category string

const (
	StatusChanged      Category = "status_changed"
	MediaTypeChanged   Category = "media_type_changed"
	LocationChanged    Category = "location_changed"
	FieldRemoved       Category = "field_removed"
	FieldAdded         Category = "field_added"
	TypeChanged        Category = "type_changed"
	ValueChanged       Category = "value_changed"
	ArrayLengthChanged Category = "array_length_changed"
	BodyChanged        Category = "body_changed"
)

// Finding is one difference between two observations. Baseline and Candidate
// are safe descriptions (status codes, media types, JSON types, sizes,
// digests); scalar values from response bodies are never included.
type Finding struct {
	Category  Category        `json:"category"`
	Path      string          `json:"path"` // RFC 6901 pointer for body findings; "" for the document root or non-body findings
	Severity  verdict.Outcome `json:"severity"`
	Baseline  string          `json:"baseline"`
	Candidate string          `json:"candidate"`
}

// IsBody reports whether the finding refers to a location inside the body.
func (f Finding) IsBody() bool {
	switch f.Category {
	case StatusChanged, MediaTypeChanged, LocationChanged:
		return false
	}
	return true
}

// Options control a comparison.
type Options struct {
	Ignore          []string // exact RFC 6901 pointers whose same-type scalar value differences are suppressed
	StrictAdditions bool     // added fields are FAIL instead of WARN
	MaxFindings     int      // 0 means DefaultMaxFindings
}

// DefaultMaxFindings caps the findings retained per comparison. The cap
// limits report size only; it never changes the verdict (see Counts).
const DefaultMaxFindings = 100

// Counts tallies every difference discovered by a comparison, whether or not
// the finding itself was retained under the cap.
type Counts struct {
	Total       int `json:"total"`
	Fail        int `json:"fail"`
	Warn        int `json:"warn"`
	Retained    int `json:"retained"`
	Omitted     int `json:"omitted"`
	OmittedFail int `json:"omitted_fail"`
	OmittedWarn int `json:"omitted_warn"`
}

// Outcome is the worst severity over ALL discovered differences, including
// omitted ones, or PASS when there are none.
func (c Counts) Outcome() verdict.Outcome {
	switch {
	case c.Fail > 0:
		return verdict.Fail
	case c.Warn > 0:
		return verdict.Warn
	}
	return verdict.Pass
}

// Result is the outcome of comparing two observations.
type Result struct {
	Findings   []Finding      // retained findings, sorted (at most MaxFindings)
	Omitted    int            // findings not retained; same as Counts.Omitted
	Counts     Counts         // every discovered difference, retained or omitted
	Suppressed map[string]int // ignore pointer -> differences it suppressed
}

// Outcome is the worst severity over every discovered difference. It is
// computed from Counts, never from the retained subset, so the finding cap
// cannot turn a FAIL into a WARN or PASS.
func (r Result) Outcome() verdict.Outcome { return r.Counts.Outcome() }

// Observed is a normalized response ready for comparison.
type Observed struct {
	Method    string
	Status    int
	MediaType string // lowercased type/subtype without parameters; "" if absent
	Location  Location
	Body      []byte
	JSON      *Value // parsed body when the media type is JSON and the body is non-empty
}

// Observe normalizes a response. It fails only when the response declares a
// JSON media type and has a non-empty body that is not valid JSON.
func Observe(method string, status int, contentType string, location string, hasLocation bool, body []byte, requestURL *url.URL) (Observed, error) {
	o := Observed{
		Method:    method,
		Status:    status,
		MediaType: NormalizeMediaType(contentType),
		Location:  NormalizeLocation(location, hasLocation, requestURL),
		Body:      body,
	}
	if method != http.MethodHead && IsJSON(o.MediaType) && len(body) > 0 {
		v, err := ParseJSON(body)
		if err != nil {
			return o, fmt.Errorf("declared %s but body is not valid JSON: %w", o.MediaType, err)
		}
		o.JSON = v
	}
	return o, nil
}

// NormalizeMediaType returns the lowercased media type without parameters, so
// "Application/JSON; charset=UTF-8" and "application/json" are equal.
func NormalizeMediaType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// IsJSON reports whether a normalized media type is JSON (application/json
// or any +json structured syntax suffix).
func IsJSON(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// Location is a normalized redirect target. Key is used for comparison;
// Display is safe to print (query values are redacted).
type Location struct {
	Present bool
	Key     string
	Display string
}

var originKey = config.OriginKey

// NormalizeLocation resolves a Location header against the request URL. A
// same-origin target is reduced to its path and query so that baseline and
// candidate redirects to their own hosts compare equal. Cross-origin targets
// keep their full URL and are never followed.
func NormalizeLocation(raw string, present bool, requestURL *url.URL) Location {
	if !present {
		return Location{}
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || requestURL == nil {
		return Location{Present: true, Key: "invalid:" + raw, Display: "(unparseable Location)"}
	}
	abs := requestURL.ResolveReference(ref)
	pathPart := abs.EscapedPath()
	if pathPart == "" {
		pathPart = "/"
	}
	key := pathPart
	display := pathPart
	if abs.RawQuery != "" || abs.ForceQuery {
		key += "?" + abs.RawQuery
		display += "?<redacted>"
	}
	if abs.Fragment != "" {
		key += "#" + abs.EscapedFragment()
	}
	if originKey(abs) == originKey(requestURL) {
		return Location{Present: true, Key: "same-origin " + key, Display: display}
	}
	host := strings.ToLower(abs.Scheme) + "://" + strings.ToLower(abs.Host)
	return Location{Present: true, Key: "cross-origin " + originKey(abs) + key, Display: "cross-origin " + host + display}
}

// Compare compares a baseline observation with a candidate observation.
func Compare(b, c Observed, opts Options) Result {
	d := newDiffer(opts)
	if b.Status != c.Status {
		d.add(StatusChanged, "", verdict.Fail, strconv.Itoa(b.Status), strconv.Itoa(c.Status))
	}
	mediaDiffers := b.MediaType != c.MediaType
	if mediaDiffers {
		d.add(MediaTypeChanged, "", verdict.Fail, orNone(b.MediaType), orNone(c.MediaType))
	}
	if (b.Location.Present || c.Location.Present) && b.Location.Key != c.Location.Key {
		d.add(LocationChanged, "", verdict.Fail, locDisplay(b.Location), locDisplay(c.Location))
	}
	if b.Method != http.MethodHead && !mediaDiffers {
		if b.JSON != nil && c.JSON != nil {
			d.walk("", b.JSON, c.JSON)
		} else if !bytes.Equal(b.Body, c.Body) {
			d.add(BodyChanged, "", verdict.Fail, describeBytes(b.Body), describeBytes(c.Body))
		}
	}
	return d.result()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func locDisplay(l Location) string {
	if !l.Present {
		return "(no Location)"
	}
	return l.Display
}

// describeBytes summarizes an opaque body without revealing its content.
func describeBytes(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%d bytes sha256:%s", len(b), hex.EncodeToString(sum[:6]))
}

type differ struct {
	opts       Options
	ignore     map[string]bool
	findings   []Finding
	counts     Counts
	suppressed map[string]int
}

func newDiffer(opts Options) *differ {
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = DefaultMaxFindings
	}
	d := &differ{opts: opts, ignore: map[string]bool{}, suppressed: map[string]int{}}
	for _, p := range opts.Ignore {
		d.ignore[p] = true
	}
	return d
}

// add records a difference. Every difference is counted; at most
// MaxFindings are retained. Once the cap is reached, a FAIL replaces the most
// recently retained WARN so that blocking evidence stays visible.
func (d *differ) add(cat Category, path string, sev verdict.Outcome, base, cand string) {
	f := Finding{Category: cat, Path: path, Severity: sev, Baseline: base, Candidate: cand}
	d.counts.Total++
	switch sev {
	case verdict.Fail:
		d.counts.Fail++
	case verdict.Warn:
		d.counts.Warn++
	}
	if len(d.findings) < d.opts.MaxFindings {
		d.findings = append(d.findings, f)
		return
	}
	if sev == verdict.Fail {
		for i := len(d.findings) - 1; i >= 0; i-- {
			if d.findings[i].Severity != verdict.Fail {
				d.omit(d.findings[i].Severity)
				d.findings[i] = f
				return
			}
		}
	}
	d.omit(sev)
}

func (d *differ) omit(sev verdict.Outcome) {
	d.counts.Omitted++
	switch sev {
	case verdict.Fail:
		d.counts.OmittedFail++
	case verdict.Warn:
		d.counts.OmittedWarn++
	}
}

// walk compares two JSON values at path. Object key order never matters;
// array order does. Ignore rules apply only to same-type scalar values at the
// exact path, so they can never hide removals, type changes or structure.
func (d *differ) walk(path string, b, c *Value) {
	if b.Kind != c.Kind {
		d.add(TypeChanged, path, verdict.Fail, b.Kind.String(), c.Kind.String())
		return
	}
	switch b.Kind {
	case Object:
		keys := make([]string, 0, len(b.Object)+len(c.Object))
		for k := range b.Object {
			keys = append(keys, k)
		}
		for k := range c.Object {
			if _, ok := b.Object[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "/" + escapeToken(k)
			bv, inB := b.Object[k]
			cv, inC := c.Object[k]
			switch {
			case !inC:
				d.add(FieldRemoved, p, verdict.Fail, bv.Kind.String(), "missing")
			case !inB:
				sev := verdict.Warn
				if d.opts.StrictAdditions {
					sev = verdict.Fail
				}
				d.add(FieldAdded, p, sev, "missing", cv.Kind.String())
			default:
				d.walk(p, bv, cv)
			}
		}
	case Array:
		n := min(len(b.Array), len(c.Array))
		for i := 0; i < n; i++ {
			d.walk(path+"/"+strconv.Itoa(i), b.Array[i], c.Array[i])
		}
		if len(b.Array) != len(c.Array) {
			d.add(ArrayLengthChanged, path, verdict.Fail, fmt.Sprintf("length %d", len(b.Array)), fmt.Sprintf("length %d", len(c.Array)))
		}
	default:
		if b.scalarEqual(c) {
			return
		}
		if d.ignore[path] {
			d.suppressed[path]++
			return
		}
		d.add(ValueChanged, path, verdict.Fail, b.Kind.String(), c.Kind.String())
	}
}

func (d *differ) result() Result {
	SortFindings(d.findings)
	d.counts.Retained = len(d.findings)
	return Result{Findings: d.findings, Omitted: d.counts.Omitted, Counts: d.counts, Suppressed: d.suppressed}
}

// SortFindings orders findings deterministically: non-body findings first
// (status, media type, location), then body findings by path and category.
func SortFindings(fs []Finding) {
	rank := func(c Category) int {
		switch c {
		case StatusChanged:
			return 0
		case MediaTypeChanged:
			return 1
		case LocationChanged:
			return 2
		}
		return 3
	}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := rank(a.Category), rank(b.Category); ra != rb {
			return ra < rb
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Category < b.Category
	})
}
