package replay

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AddysEdge/deployguard/internal/config"
)

func mustOrigin(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := config.ParseOrigin(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newTarget(t *testing.T, srv *httptest.Server) *Target {
	t.Helper()
	tg := NewTarget("test", mustOrigin(t, srv.URL), 4)
	t.Cleanup(tg.Close)
	return tg
}

func get(path string) Request {
	return Request{Method: http.MethodGet, Path: path, Query: url.Values{}, Timeout: 2 * time.Second, MaxBytes: 1024}
}

func TestBuildURL(t *testing.T) {
	origin := mustOrigin(t, "http://127.0.0.1:8080")
	u, err := BuildURL(origin, "/a%2Fb/c", url.Values{"z": {"1"}, "a": {"x y"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.String(), "http://127.0.0.1:8080/a%2Fb/c?a=x+y&z=1"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	for _, bad := range []string{"relative", "//evil.example/x", "/x?y=1", "/x#f"} {
		if _, err := BuildURL(origin, bad, nil); err == nil {
			t.Errorf("BuildURL(%q) should fail", bad)
		}
	}
}

func TestDoCapturesResponseAndSendsHeaders(t *testing.T) {
	var gotAuth, gotUA, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotQuery = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	req := get("/thing")
	req.Query = url.Values{"q": {"pen"}}
	req.Headers = []config.Header{config.NewHeader("Authorization", "Bearer abc")}
	resp := newTarget(t, srv).Do(context.Background(), req, true)
	if !resp.OK() {
		t.Fatalf("unexpected failure: %v", resp.Failure)
	}
	if resp.Status != 200 || string(resp.Body) != `{"ok":true}` || resp.BodyBytes != 11 || resp.Duration <= 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if !strings.HasPrefix(resp.ContentType, "application/json") {
		t.Fatalf("content type %q", resp.ContentType)
	}
	if gotAuth != "Bearer abc" || gotUA != UserAgent || gotQuery != "q=pen" {
		t.Fatalf("server saw auth=%q ua=%q query=%q", gotAuth, gotUA, gotQuery)
	}
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/from", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/to", http.StatusFound)
	})
	mux.HandleFunc("/to", func(w http.ResponseWriter, r *http.Request) { followed.Store(true) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := newTarget(t, srv).Do(context.Background(), get("/from"), true)
	if !resp.OK() || resp.Status != http.StatusFound || !resp.HasLocation || resp.Location != "/to" {
		t.Fatalf("unexpected: %+v", resp)
	}
	if followed.Load() {
		t.Fatal("redirect was followed")
	}
}

func TestDoBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 1024+len(r.URL.Query().Get("extra")))))
	}))
	defer srv.Close()
	tg := newTarget(t, srv)

	exact := tg.Do(context.Background(), get("/"), true)
	if !exact.OK() || len(exact.Body) != 1024 {
		t.Fatalf("exact-limit body should pass: %+v", exact.Failure)
	}
	over := get("/")
	over.Query = url.Values{"extra": {"y"}}
	resp := tg.Do(context.Background(), over, true)
	if resp.Failure == nil || resp.Failure.Kind != FailBodyLimit || resp.Body != nil {
		t.Fatalf("want body_limit failure, got %+v", resp.Failure)
	}
	// Discarding mode enforces the same limit.
	if resp := tg.Do(context.Background(), over, false); resp.Failure == nil || resp.Failure.Kind != FailBodyLimit {
		t.Fatalf("discard mode: want body_limit, got %+v", resp.Failure)
	}
}

func TestDoHeadReadsNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Write([]byte("a,b\n"))
	}))
	defer srv.Close()
	req := get("/")
	req.Method = http.MethodHead
	resp := newTarget(t, srv).Do(context.Background(), req, true)
	if !resp.OK() || resp.Status != 200 || len(resp.Body) != 0 || resp.ContentType != "text/csv" {
		t.Fatalf("unexpected HEAD response: %+v", resp)
	}
}

func TestDoTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	req := get("/")
	req.Timeout = 50 * time.Millisecond
	resp := newTarget(t, srv).Do(context.Background(), req, true)
	if resp.Failure == nil || resp.Failure.Kind != FailTimeout {
		t.Fatalf("want timeout, got %+v", resp.Failure)
	}
	if resp.Duration < 50*time.Millisecond || resp.Duration > 2*time.Second {
		t.Fatalf("timeout duration out of range: %s", resp.Duration)
	}
}

func TestDoTimeoutDuringBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	req := get("/")
	req.Timeout = 80 * time.Millisecond
	resp := newTarget(t, srv).Do(context.Background(), req, true)
	if resp.Failure == nil || resp.Failure.Kind != FailTimeout {
		t.Fatalf("want timeout while reading body, got %+v", resp.Failure)
	}
}

func TestDoConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tg := NewTarget("dead", mustOrigin(t, "http://"+addr), 1)
	defer tg.Close()
	resp := tg.Do(context.Background(), get("/"), true)
	if resp.Failure == nil || resp.Failure.Kind != FailRefused {
		t.Fatalf("want connection_refused, got %+v", resp.Failure)
	}
}

func TestDoDNSFailure(t *testing.T) {
	tg := NewTarget("dns", mustOrigin(t, "http://deployguard-does-not-exist.invalid"), 1)
	defer tg.Close()
	resp := tg.Do(context.Background(), get("/"), true)
	if resp.Failure == nil || resp.Failure.Kind != FailDNS {
		t.Fatalf("want dns failure, got %+v", resp.Failure)
	}
}

func TestDoTLSVerificationFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// The test server's self-signed certificate is not trusted: verification
	// must fail rather than be skipped.
	resp := newTarget(t, srv).Do(context.Background(), get("/"), true)
	if resp.Failure == nil || resp.Failure.Kind != FailTLS {
		t.Fatalf("want tls failure, got %+v", resp.Failure)
	}
}

func TestDoConnectionDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	resp := newTarget(t, srv).Do(context.Background(), get("/"), true)
	if resp.Failure == nil || (resp.Failure.Kind != FailNetwork && resp.Failure.Kind != FailReset) {
		t.Fatalf("want network/reset failure, got %+v", resp.Failure)
	}
}

func TestDoCanceledByParent(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	resp := newTarget(t, srv).Do(ctx, get("/"), true)
	if resp.Failure == nil || resp.Failure.Kind != FailCanceled {
		t.Fatalf("want canceled, got %+v", resp.Failure)
	}
}

func TestFailureMessagesOmitURLAndHeaders(t *testing.T) {
	tg := NewTarget("dead", mustOrigin(t, "http://127.0.0.1:1"), 1)
	defer tg.Close()
	req := get("/secret-path")
	req.Query = url.Values{"q": {"value"}}
	req.Headers = []config.Header{config.NewHeader("Authorization", "Bearer TOPSECRET")}
	resp := tg.Do(context.Background(), req, true)
	if resp.Failure == nil {
		t.Fatal("expected failure")
	}
	msg := resp.Failure.Error()
	if strings.Contains(msg, "TOPSECRET") || strings.Contains(msg, "secret-path") || strings.Contains(msg, "q=value") {
		t.Fatalf("failure message leaks request details: %s", msg)
	}
}

func TestForEachBoundsInFlightRequests(t *testing.T) {
	const limit, total = 3, 24
	var inFlight, maxSeen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inFlight.Add(-1)
	}))
	defer srv.Close()
	tg := newTarget(t, srv)

	var done atomic.Int64
	started := ForEach(context.Background(), total, limit, func(ctx context.Context, i int) {
		if tg.Do(ctx, get("/"), false).OK() {
			done.Add(1)
		}
	})
	if started != total || done.Load() != total {
		t.Fatalf("started=%d done=%d want %d", started, done.Load(), total)
	}
	if got := maxSeen.Load(); got > limit {
		t.Fatalf("observed %d in-flight requests, limit is %d", got, limit)
	} else if got < 2 {
		t.Fatalf("expected concurrent execution, max in flight was %d", got)
	}
}

func TestForEachCancellationStopsAndJoinsWorkers(t *testing.T) {
	before := runtime.NumGoroutine()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done() // block until the client gives up
	}))
	tg := NewTarget("slow", mustOrigin(t, srv.URL), 4)

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	kinds := map[FailureKind]int{}
	go func() {
		for hits.Load() < 4 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	begin := time.Now()
	started := ForEach(ctx, 1000, 4, func(ctx context.Context, i int) {
		req := get("/")
		req.Timeout = 10 * time.Second
		resp := tg.Do(ctx, req, false)
		mu.Lock()
		if resp.Failure != nil {
			kinds[resp.Failure.Kind]++
		}
		mu.Unlock()
	})
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	if started >= 1000 || started < 4 {
		t.Fatalf("started=%d; cancellation should stop scheduling", started)
	}
	if kinds[FailCanceled] != started {
		t.Fatalf("every started request should report canceled: %v (started %d)", kinds, started)
	}

	tg.Close()
	srv.Close()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, after)
	}
}

func TestForEachEdgeCases(t *testing.T) {
	if n := ForEach(context.Background(), 0, 4, func(context.Context, int) { t.Fatal("called") }); n != 0 {
		t.Fatal("n=0 should start nothing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := ForEach(ctx, 10, 4, func(context.Context, int) { t.Fatal("called") }); n != 0 {
		t.Fatal("canceled context should start nothing")
	}
	var seen [5]atomic.Bool
	ForEach(context.Background(), 5, 0, func(_ context.Context, i int) { seen[i].Store(true) })
	for i := range seen {
		if !seen[i].Load() {
			t.Fatalf("index %d not visited", i)
		}
	}
}
