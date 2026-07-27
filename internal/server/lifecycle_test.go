package bleephub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// lifecycleServer starts a server of its own on a free port and returns it with
// a cancel that stops it. The shared harness server cannot be used here: these
// tests shut a server down, and every other test in the package needs one that
// stays up.
func lifecycleServer(t *testing.T) (base string, cancel func(), done <-chan error) {
	t.Helper()
	// The shared harness points this at its own SSH port; a second server
	// binding it would fail to start and never become ready.
	t.Setenv("BLEEPHUB_SSH_ADDR", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	srv := NewServer(addr, zerolog.New(io.Discard))
	ctx, stop := context.WithCancel(context.Background())
	exit := make(chan error, 1)
	go func() { exit <- srv.ListenAndServe(ctx) }()

	base = "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			return base, stop, exit
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	t.Fatalf("server never became ready on %s", addr)
	return "", nil, nil
}

// A request already in flight when the signal arrives must finish. Cutting it
// is what happens with no handler at all: the runtime sends SIGTERM, the
// process ignores it, and SIGKILL lands mid-response.
func TestShutdownDrainsAnInFlightRequest(t *testing.T) {
	base, stop, done := lifecycleServer(t)

	// Hold a request open, then shut down while it is still being served.
	type result struct {
		status int
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/api/v3/zen")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the request failed before shutdown was even requested: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("pre-shutdown request = %d, want 200", r.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never completed")
	}

	stop()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe returned %v, want a clean stop", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the server never returned from shutdown")
	}
}

// The drain is bounded. This server holds long-poll requests open by design, so
// an unbounded wait for connections to close would mean never stopping — and a
// container that will not stop is SIGKILLed, after which nothing drains at all.
func TestShutdownIsBoundedByItsGrace(t *testing.T) {
	base, stop, done := lifecycleServer(t)

	// A broker long-poll is the shape that would otherwise hold shutdown open.
	// It needs no credential to be *started*; being refused is fine, what
	// matters is that the connection exists across the shutdown.
	conn, err := net.Dial("tcp", base[len("http://"):])
	if err != nil {
		t.Fatalf("open a connection to hold across shutdown: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /_apis/v1/Message/1 HTTP/1.1\r\nHost: %s\r\n\r\n", base[len("http://"):])

	start := time.Now()
	stop()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatalf("shutdown exceeded its %s grace by more than 10s — the bound is not being applied", shutdownGrace)
	}
	if elapsed := time.Since(start); elapsed > shutdownGrace+5*time.Second {
		t.Errorf("shutdown took %s against a %s grace", elapsed, shutdownGrace)
	}
}

// The cron dispatcher used to be `for { time.Sleep }` with no owner, so it ran
// until the process died. Shutdown must be able to stop it, and must wait for
// it rather than returning while it still runs.
func TestShutdownStopsTheScheduleDispatcher(t *testing.T) {
	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	ctx, stop := context.WithCancel(context.Background())
	srv.startScheduleDispatcher(ctx)

	stopped := make(chan struct{})
	go func() {
		srv.background.Wait()
		close(stopped)
	}()

	// Nothing should have finished while the context is live.
	select {
	case <-stopped:
		t.Fatal("the dispatcher exited without being asked to")
	case <-time.After(50 * time.Millisecond):
	}

	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatcher did not stop when its context was cancelled — it is sleeping through shutdown")
	}
}

// startGitSSH's accept loop had no owner either, and its listener was never
// closed. Both are checked here because the loop only unblocks when the
// listener does.
func TestShutdownClosesTheSSHListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	// Generated here rather than read from a fixture: a skipped test proves
	// nothing, and this one is about shutdown, not about key handling.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a host key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal the host key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	t.Setenv("BLEEPHUB_SSH_ADDR", addr)
	t.Setenv("BLEEPHUB_SSH_HOST_KEY", string(pemKey))

	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	ctx, stop := context.WithCancel(context.Background())
	if err := srv.startGitSSH(ctx); err != nil {
		t.Fatalf("startGitSSH: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err != nil {
		t.Fatalf("the SSH listener never came up: %v", err)
	} else {
		conn.Close()
	}

	stopped := make(chan struct{})
	go func() {
		srv.background.Wait()
		close(stopped)
	}()
	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSH accept loop did not stop — its listener is never closed")
	}
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		conn.Close()
		t.Error("the SSH port still accepts connections after shutdown")
	}
}
