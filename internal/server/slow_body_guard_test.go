package bleephub

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSlowBodyGuardDropsStalledUpload covers CORE-010: the header-read Slowloris
// is already bounded by ReadHeaderTimeout, but a slow *body* — a client that
// announces a Content-Length and then trickles or stalls the payload — held a
// connection and goroutine indefinitely, because a fixed ReadTimeout is
// deliberately unset (it would cut off large git pushes). The slowBodyGuard
// resets a sliding inactivity deadline before every body read: a client making
// steady progress is unaffected, a stalled one is dropped.
func TestSlowBodyGuardDropsStalledUpload(t *testing.T) {
	const idle = 250 * time.Millisecond
	handler := slowBodyGuard(idle, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "slow body", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// A promptly delivered body is served normally — the guard is an inactivity
	// bound, not a total-time cap.
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("fast POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "OK" {
		t.Fatalf("fast POST = %d %q, want 200 OK", resp.StatusCode, body)
	}

	// A body that announces 20 bytes, sends a few, then stalls past the
	// inactivity deadline must be dropped — never served a 200.
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: %s\r\nContent-Length: 20\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n", addr)
	if _, err := conn.Write([]byte("12345")); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	time.Sleep(idle * 4) // stall well past the inactivity deadline

	_, _ = conn.Write([]byte("67890")) // too late; may already be reset

	// Read whatever the server returns, bounded by a timer rather than a
	// wall-clock read deadline (the test-clock gate forbids time.Now in tests).
	// The server should have closed the connection when the deadline fired, so
	// this returns promptly; the timer only guards against a hang.
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(conn)
		done <- string(b)
	}()
	var got string
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
	}
	if strings.Contains(got, "200 OK") {
		t.Fatalf("a stalled slow-body upload was served a 200 response: %q", got)
	}
}
