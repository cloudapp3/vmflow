package controlapi

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitHeader(t *testing.T) {
	cases := []struct {
		in, n, v string
		ok       bool
	}{
		{"Name: Value", "Name", "Value", true},
		{"Name=Value", "Name", "Value", true},
		{"  Name :  Value  ", "Name", "Value", true},
		{"", "", "", false},
		{"noseparator", "", "", false},
	}
	for _, c := range cases {
		n, v, ok := splitHeader(c.in)
		if n != c.n || v != c.v || ok != c.ok {
			t.Errorf("splitHeader(%q) = (%q,%q,%v) want (%q,%q,%v)", c.in, n, v, ok, c.n, c.v, c.ok)
		}
	}
}

func TestHeaderFlagsParseAndApply(t *testing.T) {
	t.Setenv("VMFLOW_HEADERS", "")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	hd := AddHeaderFlags(fs)
	if err := fs.Parse([]string{"-H", "X-A: 1", "--header", "X-B=2", "-H", "X-A: 3"}); err != nil {
		t.Fatal(err)
	}
	if !hd.Any() {
		t.Fatal("expected headers after parse")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	hd.Apply(req)
	if got := req.Header.Get("X-A"); got != "3" { // later entry wins per name
		t.Fatalf("X-A = %q, want 3", got)
	}
	if got := req.Header.Get("X-B"); got != "2" {
		t.Fatalf("X-B = %q, want 2", got)
	}
}

func TestHeaderFlagsHTTPHeader(t *testing.T) {
	hd := HeaderFlags{"X-A: 1", "X-B=2", "garbage", ""}
	h := hd.HTTPHeader()
	if h.Get("X-A") != "1" || h.Get("X-B") != "2" {
		t.Fatalf("unexpected: %v", h)
	}
	for k := range h {
		if k != "X-A" && k != "X-B" {
			t.Fatalf("unexpected header %q", k)
		}
	}
}
