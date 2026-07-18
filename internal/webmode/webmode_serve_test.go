package webmode

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// freePort grabs an ephemeral port and releases it for Run to rebind.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// TestWebModeServesQuickly proves that Run — including the desktop-KV seed added
// to it — binds and serves HTTP promptly, and does not hang on startup.
func TestWebModeServesQuickly(t *testing.T) {
	t.Setenv("TOLLECODE_HOME", t.TempDir())
	port := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- Run(ctx, port, false) }()

	// Probe an API route rather than "/": the Angular UI is staged into
	// dist/browser at release time, so "/" only serves 200 in builds that have it.
	// /web/access/status is mounted unconditionally and proves the same thing this
	// test cares about — the server bound and is routing, without hanging.
	url := fmt.Sprintf("http://127.0.0.1:%d/web/access/status", port)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-errCh:
			t.Fatalf("Run exited early: %v", e)
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Logf("served in %v", time.Since(start))
				cancel()
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("web server did not serve HTTP 200 within 8s — startup is blocked")
}
