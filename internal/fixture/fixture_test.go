package fixture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fetch(t *testing.T, v Variant, path string) (int, string, http.Header) {
	t.Helper()
	srv := httptest.NewServer(NewHandler(Options{Variant: v, SlowDelay: 5 * time.Millisecond}))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestSeededDifferences(t *testing.T) {
	tests := []struct {
		path     string
		variant  Variant
		wantCode int
		contains string
		excludes string
	}{
		{"/api/users/42", Baseline, 200, `"email"`, ""},
		{"/api/users/42", Clean, 200, `"email"`, ""},
		{"/api/users/42", Regressed, 200, `"name"`, `"email"`},
		{"/api/orders/7", Regressed, 200, `"total":"129.50"`, ""},
		{"/api/items/3", Regressed, 404, "not found", ""},
		{"/api/items/3", Clean, 200, "Widget", ""},
		{"/api/inventory", Regressed, 500, "internal", ""},
		{"/api/catalog", Regressed, 200, `"featured"`, ""},
		{"/api/catalog", Clean, 200, "", `"featured"`},
		{"/api/profile", Baseline, 200, `{"id":9`, ""},
		{"/api/profile", Clean, 200, `{"settings"`, ""},
		{"/health", Clean, 200, `"variant":"clean"`, ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.variant)+tt.path, func(t *testing.T) {
			code, body, _ := fetch(t, tt.variant, tt.path)
			if code != tt.wantCode || !strings.Contains(body, tt.contains) || (tt.excludes != "" && strings.Contains(body, tt.excludes)) {
				t.Fatalf("got %d %s", code, body)
			}
		})
	}
}

func TestRedirectIsSameOriginAbsolute(t *testing.T) {
	code, _, h := fetch(t, Baseline, "/api/redirect")
	if code != http.StatusFound || !strings.HasPrefix(h.Get("Location"), "http://127.0.0.1:") {
		t.Fatalf("got %d Location=%q", code, h.Get("Location"))
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	if code, _, _ := fetch(t, Baseline, "/nope"); code != 404 {
		t.Fatalf("unknown route: %d", code)
	}
	if _, err := ParseVariant("weird"); err == nil {
		t.Fatal("ParseVariant should reject unknown variants")
	}
}
