// Package fixture is a tiny demo API used only to demonstrate and test
// DeployGuard. One handler serves three variants:
//
//   - baseline:  the current release
//   - clean:     a harmless candidate (reordered JSON keys, different
//     Content-Type casing, small latency jitter) that must not FAIL
//   - regressed: a candidate with one seeded regression per endpoint
//
// Each seeded case lives on its own endpoint so it can be evaluated
// independently:
//
//	R1 /api/users/42    regressed removes "email"
//	R2 /api/orders/7    regressed turns "total" from number into string
//	R3 /api/items/3     regressed returns 404 instead of 200
//	R4 /api/inventory   regressed returns 500
//	R5 /api/search      regressed adds a large fixed delay
//	R6 /api/status      every variant returns a fresh requestId/timestamp
//	R7 /api/profile     candidates emit object keys in a different order
//	R8 /api/catalog     regressed adds a harmless "featured" field
//	I1 /api/unstable    every variant returns a per-request counter
//	N1 /api/noisy       candidates add a few milliseconds of jitter
package fixture

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"
)

// Variant selects fixture behavior.
type Variant string

const (
	Baseline  Variant = "baseline"
	Clean     Variant = "clean"
	Regressed Variant = "regressed"
)

// ParseVariant validates a variant name.
func ParseVariant(s string) (Variant, error) {
	switch v := Variant(s); v {
	case Baseline, Clean, Regressed:
		return v, nil
	}
	return "", fmt.Errorf("unknown variant %q (baseline, clean or regressed)", s)
}

// Options tune the fixture.
type Options struct {
	Variant   Variant
	SlowDelay time.Duration // R5 delay added by the regressed variant
	NoiseMax  time.Duration // N1 maximum jitter added by candidate variants
}

// DefaultSlowDelay and DefaultNoiseMax are the documented fixture defaults.
const (
	DefaultSlowDelay = 120 * time.Millisecond
	DefaultNoiseMax  = 4 * time.Millisecond
)

type server struct {
	opts    Options
	counter atomic.Int64
}

// NewHandler returns the fixture API for one variant.
func NewHandler(opts Options) http.Handler {
	if opts.SlowDelay == 0 {
		opts.SlowDelay = DefaultSlowDelay
	}
	if opts.NoiseMax == 0 {
		opts.NoiseMax = DefaultNoiseMax
	}
	s := &server{opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/users/42", s.user)
	mux.HandleFunc("GET /api/orders/7", s.order)
	mux.HandleFunc("GET /api/items/3", s.item)
	mux.HandleFunc("GET /api/inventory", s.inventory)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/profile", s.profile)
	mux.HandleFunc("GET /api/catalog", s.catalog)
	mux.HandleFunc("GET /api/export.csv", s.export)
	mux.HandleFunc("GET /api/redirect", s.redirect)
	mux.HandleFunc("GET /api/unstable", s.unstable)
	mux.HandleFunc("GET /api/noisy", s.noisy)
	return mux
}

func (s *server) candidate() bool { return s.opts.Variant != Baseline }
func (s *server) regressed() bool { return s.opts.Variant == Regressed }

// writeJSON writes a literal JSON document so key order is controlled.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, fmt.Sprintf(`{"status":"ok","variant":%q}`, s.opts.Variant))
}

func (s *server) user(w http.ResponseWriter, _ *http.Request) {
	if s.regressed() {
		writeJSON(w, 200, `{"id":42,"name":"Ada Lovelace","roles":["admin","editor"],"active":true,"manager":null}`)
		return
	}
	writeJSON(w, 200, `{"id":42,"name":"Ada Lovelace","email":"ada@example.com","roles":["admin","editor"],"active":true,"manager":null}`)
}

func (s *server) order(w http.ResponseWriter, _ *http.Request) {
	total := `129.50`
	if s.regressed() {
		total = `"129.50"`
	} else if s.candidate() {
		total = `129.5` // numerically equal: must not be a difference
	}
	writeJSON(w, 200, `{"id":7,"total":`+total+`,"currency":"USD","items":[{"sku":"A-1","qty":2},{"sku":"B-9","qty":1}]}`)
}

func (s *server) item(w http.ResponseWriter, _ *http.Request) {
	if s.regressed() {
		writeJSON(w, 404, `{"error":"not found"}`)
		return
	}
	writeJSON(w, 200, `{"id":3,"name":"Widget","price":9.99}`)
}

func (s *server) inventory(w http.ResponseWriter, _ *http.Request) {
	if s.regressed() {
		writeJSON(w, 500, `{"error":"internal"}`)
		return
	}
	writeJSON(w, 200, `{"warehouse":"east","count":17}`)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	if s.regressed() {
		time.Sleep(s.opts.SlowDelay)
	}
	writeJSON(w, 200, fmt.Sprintf(`{"query":%q,"results":[{"id":1,"title":"Pen"},{"id":2,"title":"Pencil"}]}`, r.URL.Query().Get("q")))
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	var id [8]byte
	rand.Read(id[:])
	writeJSON(w, 200, fmt.Sprintf(`{"service":"catalog","healthy":true,"requestId":%q,"timestamp":%q}`,
		hex.EncodeToString(id[:]), time.Now().UTC().Format(time.RFC3339Nano)))
}

func (s *server) profile(w http.ResponseWriter, _ *http.Request) {
	if s.candidate() {
		writeJSON(w, 200, `{"settings":{"lang":"en","theme":"dark"},"display":"ada","id":9}`)
		return
	}
	writeJSON(w, 200, `{"id":9,"display":"ada","settings":{"theme":"dark","lang":"en"}}`)
}

func (s *server) catalog(w http.ResponseWriter, _ *http.Request) {
	if s.regressed() {
		writeJSON(w, 200, `{"items":[{"id":1,"title":"Pen"}],"featured":true}`)
		return
	}
	writeJSON(w, 200, `{"items":[{"id":1,"title":"Pen"}]}`)
}

func (s *server) export(w http.ResponseWriter, _ *http.Request) {
	ct := "text/csv; charset=utf-8"
	if s.candidate() {
		ct = "Text/CSV;charset=UTF-8" // formatting only: same media type
	}
	w.Header().Set("Content-Type", ct)
	io.WriteString(w, "id,title\n1,Pen\n2,Pencil\n")
}

// redirect returns an absolute same-origin Location, so baseline and
// candidate Locations differ only by their own host.
func (s *server) redirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", "http://"+r.Host+"/api/users/42")
	w.WriteHeader(http.StatusFound)
}

func (s *server) unstable(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, fmt.Sprintf(`{"value":%d}`, s.counter.Add(1)))
}

func (s *server) noisy(w http.ResponseWriter, _ *http.Request) {
	if s.candidate() {
		// Uniform jitter in [0, NoiseMax]: a modest, noisy difference.
		n, err := rand.Int(rand.Reader, big.NewInt(int64(s.opts.NoiseMax)+1))
		if err == nil {
			time.Sleep(time.Duration(n.Int64()))
		}
	}
	writeJSON(w, 200, `{"ok":true}`)
}
