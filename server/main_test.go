package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPSRedirect(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := securityHeaders(ok, true, "")
	cases := []struct {
		name, header, value string
		code                int
		hsts                bool
	}{
		{"cf-visitor http", "CF-Visitor", `{"scheme":"http"}`, 301, false},
		{"x-forwarded-proto http", "X-Forwarded-Proto", "http", 301, false},
		{"cf-visitor https", "CF-Visitor", `{"scheme":"https"}`, 200, true},
		{"local (no headers)", "", "", 200, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://pythonbpf.xeon.me/about?x=1", nil)
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s: code %d, want %d", c.name, w.Code, c.code)
		}
		if c.code == 301 && w.Header().Get("Location") != "https://pythonbpf.xeon.me/about?x=1" {
			t.Errorf("%s: Location %q", c.name, w.Header().Get("Location"))
		}
		if got := w.Header().Get("Strict-Transport-Security") != ""; got != c.hsts {
			t.Errorf("%s: HSTS present=%v, want %v", c.name, got, c.hsts)
		}
	}
	// Without -trust-cf the headers are ignored (dev mode, direct access).
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.Header.Set("CF-Visitor", `{"scheme":"http"}`)
	w := httptest.NewRecorder()
	securityHeaders(ok, false, "").ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("untrusted: code %d", w.Code)
	}
}

func TestFrameAncestors(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for in, want := range map[string]string{
		"":                            "frame-ancestors 'none';",
		"https://pythonbpf.github.io": "frame-ancestors https://pythonbpf.github.io;",
	} {
		w := httptest.NewRecorder()
		securityHeaders(ok, false, in).ServeHTTP(w, httptest.NewRequest("GET", "http://x/", nil))
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, want) {
			t.Errorf("frame-ancestors %q: CSP %q lacks %q", in, csp, want)
		}
	}
}
