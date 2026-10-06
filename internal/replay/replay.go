// Package replay executes the configured GET/HEAD requests against one target
// origin with strict limits: per-request deadlines, bounded response bodies,
// no redirect following and no retries. Failures are classified so that a
// timeout, DNS failure, refused connection or oversized body stays
// distinguishable in findings and reports.
package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
)

// UserAgent is sent unless the scenario configures its own User-Agent.
const UserAgent = "deployguard/1"

// Request is a target-independent request shape. The same Request is sent to
// the baseline and the candidate.
type Request struct {
	Method   string
	Path     string // root-relative, validated by config
	Query    url.Values
	Headers  []config.Header
	Timeout  time.Duration
	MaxBytes int64
}

// RequestFor derives the request for a scenario.
func RequestFor(sc config.Scenario, maxBytes int64) Request {
	return Request{
		Method:   sc.Method,
		Path:     sc.Path,
		Query:    sc.Query,
		Headers:  sc.Headers,
		Timeout:  sc.Timeout,
		MaxBytes: maxBytes,
	}
}

// Response is one observation of a target. Duration covers the time from
// immediately before the request is sent until the bounded body has been
// fully read (or the failure occurred).
type Response struct {
	Status      int
	ContentType string
	Location    string
	HasLocation bool
	Body        []byte // nil when the body was discarded or the method is HEAD
	BodyBytes   int64
	Duration    time.Duration
	Failure     *Failure
}

// OK reports whether the exchange completed without a transport or limit failure.
func (r Response) OK() bool { return r.Failure == nil }

// Target is one origin (baseline or candidate) with its own reusable client.
type Target struct {
	Name   string
	Origin *url.URL
	client *http.Client
}

// NewTarget creates a target whose transport keeps up to maxConns idle
// connections. Redirects are never followed; TLS verification uses the
// system defaults and cannot be disabled.
func NewTarget(name string, origin *url.URL, maxConns int) *Target {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = maxConns
	tr.MaxIdleConnsPerHost = maxConns
	return &Target{
		Name:   name,
		Origin: origin,
		client: &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Close releases idle connections.
func (t *Target) Close() { t.client.CloseIdleConnections() }

// BuildURL joins a validated root-relative path and query onto an origin
// without string concatenation, and refuses anything that would change the
// origin's scheme or host.
func BuildURL(origin *url.URL, path string, query url.Values) (*url.URL, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("path must be root-relative")
	}
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("malformed path")
	}
	if ref.Scheme != "" || ref.Host != "" || ref.User != nil || ref.Opaque != "" || ref.RawQuery != "" || ref.Fragment != "" {
		return nil, fmt.Errorf("path must not carry a scheme, host, query or fragment")
	}
	u := &url.URL{
		Scheme:   origin.Scheme,
		Host:     origin.Host,
		Path:     ref.Path,
		RawPath:  ref.RawPath,
		RawQuery: query.Encode(),
	}
	if u.Scheme != origin.Scheme || u.Host != origin.Host {
		return nil, fmt.Errorf("constructed URL does not match the target origin")
	}
	return u, nil
}

// Do sends req to the target once. When keepBody is false the body is still
// read (bounded) so that durations include transfer time, but it is
// discarded. Do never retries and never follows redirects.
func (t *Target) Do(ctx context.Context, req Request, keepBody bool) Response {
	u, err := BuildURL(t.Origin, req.Path, req.Query)
	if err != nil {
		return Response{Failure: &Failure{Kind: FailInvalidRequest, Message: err.Error()}}
	}
	reqCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	hr, err := http.NewRequestWithContext(reqCtx, req.Method, u.String(), nil)
	if err != nil {
		return Response{Failure: &Failure{Kind: FailInvalidRequest, Message: "could not construct request"}}
	}
	hr.Header.Set("User-Agent", UserAgent)
	for _, h := range req.Headers {
		hr.Header.Set(h.Name, h.Value())
	}

	start := time.Now()
	resp, err := t.client.Do(hr)
	if err != nil {
		return Response{Duration: time.Since(start), Failure: classify(ctx, err, req.Timeout)}
	}
	defer resp.Body.Close()

	out := Response{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}
	if loc, ok := resp.Header["Location"]; ok && len(loc) > 0 {
		out.Location, out.HasLocation = loc[0], true
	}

	if req.Method != http.MethodHead {
		limited := io.LimitReader(resp.Body, req.MaxBytes+1)
		var n int64
		if keepBody {
			var body []byte
			body, err = io.ReadAll(limited)
			n = int64(len(body))
			out.Body = body
		} else {
			n, err = io.Copy(io.Discard, limited)
		}
		out.BodyBytes = n
		switch {
		case err != nil:
			out.Failure = classify(ctx, err, req.Timeout)
		case n > req.MaxBytes:
			out.Body = nil
			out.Failure = &Failure{Kind: FailBodyLimit, Message: fmt.Sprintf("response body exceeded the %d byte limit", req.MaxBytes)}
		}
	}
	out.Duration = time.Since(start)
	return out
}
