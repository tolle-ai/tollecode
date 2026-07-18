package agent

import (
	"errors"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"example.com":              "https://example.com",
		"www.example.com/x":        "https://www.example.com/x",
		"  example.com  ":          "https://example.com",
		"http://example.com":       "http://example.com",
		"https://example.com":      "https://example.com",
		"about:blank":              "about:blank",
		"data:text/html,<p>hi</p>": "data:text/html,<p>hi</p>",
		"localhost:3000":           "http://localhost:3000",
		"127.0.0.1:8080/app":       "http://127.0.0.1:8080/app",
		"":                         "",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeURL(42); got != "" {
		t.Errorf("normalizeURL(non-string) = %q, want empty", got)
	}
}

func TestIsConnErr(t *testing.T) {
	conn := []string{
		"websocket: close 1006 (abnormal closure)",
		"could not dial: connection refused",
		"target closed",
		"page crash",
		"context canceled",
		"session with given id not found",
	}
	for _, m := range conn {
		if !isConnErr(errors.New(m)) {
			t.Errorf("isConnErr(%q) = false, want true", m)
		}
	}
	// A plain timeout or missing element must NOT be treated as a disconnect —
	// reconnecting wouldn't help and would just retry the timeout.
	notConn := []string{
		"context deadline exceeded",
		"could not find node with given selector",
		"",
	}
	for _, m := range notConn {
		if isConnErr(errors.New(m)) {
			t.Errorf("isConnErr(%q) = true, want false", m)
		}
	}
	if isConnErr(nil) {
		t.Error("isConnErr(nil) = true, want false")
	}
}

func TestJSStrEscaping(t *testing.T) {
	// Selectors with quotes/backslashes must produce a valid, injection-safe JS literal.
	if got := jsStr(`a"b`); got != `"a\"b"` {
		t.Errorf("jsStr(`a\"b`) = %s", got)
	}
	if got := jsStr(`input[name="q"]`); got != `"input[name=\"q\"]"` {
		t.Errorf("jsStr = %s", got)
	}
}

func TestGetSessionStableAndDestroy(t *testing.T) {
	const id = "test-session-stability"
	DestroyBrowserSession(id) // clean slate

	a := getSession(id)
	b := getSession(id)
	if a != b {
		t.Fatal("getSession returned different objects for the same id")
	}
	if c := getSession(id + "-other"); c == a {
		t.Fatal("getSession returned the same object for different ids")
	}

	// Simulate an established (but non-connected) session, then destroy it.
	DestroyBrowserSession(id)
	if a.closed != true {
		t.Error("DestroyBrowserSession did not mark the session closed")
	}
	// After destroy, a brand-new object should be minted.
	if d := getSession(id); d == a {
		t.Error("getSession returned the destroyed object; expected a fresh one")
	}
	DestroyBrowserSession(id)
	DestroyBrowserSession(id + "-other")
}
