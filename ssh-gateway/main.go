package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxConcurrentConnections = 32
	maxConnectionsPerMinute  = 10
	startupTimeout           = 90 * time.Second
	bannerTimeout            = 5 * time.Second
	connectionIdleTimeout    = 10 * time.Minute
	rateLimiterSweepInterval = time.Minute
)

type sourceRateLimiter struct {
	mu        sync.Mutex
	requests  map[string][]time.Time
	lastSweep time.Time
}

func (l *sourceRateLimiter) allow(address string, now time.Time) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	cutoff := now.Add(-time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= rateLimiterSweepInterval {
		for source, timestamps := range l.requests {
			firstLive := 0
			for firstLive < len(timestamps) && timestamps[firstLive].Before(cutoff) {
				firstLive++
			}
			if firstLive == len(timestamps) {
				delete(l.requests, source)
				continue
			}
			l.requests[source] = append([]time.Time(nil), timestamps[firstLive:]...)
		}
		l.lastSweep = now
	}
	recent := l.requests[host]
	start := 0
	for start < len(recent) && recent[start].Before(cutoff) {
		start++
	}
	recent = recent[start:]
	if len(recent) >= maxConnectionsPerMinute {
		l.requests[host] = recent
		return false
	}
	l.requests[host] = append(recent, now)
	return true
}

func main() {
	listenAddress := valueOr("BLEEPHUB_SSH_GATEWAY_ADDR", ":2222")
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatalf("listen SSH gateway: %v", err)
	}
	defer func() { _ = listener.Close() }()

	limiter := &sourceRateLimiter{requests: make(map[string][]time.Time)}
	connections := make(chan struct{}, maxConcurrentConnections)
	for {
		connection, err := listener.Accept()
		if err != nil {
			log.Printf("accept SSH connection: %v", err)
			continue
		}
		if !limiter.allow(connection.RemoteAddr().String(), time.Now()) {
			_ = connection.Close()
			continue
		}
		select {
		case connections <- struct{}{}:
			go func() {
				defer func() { <-connections }()
				handle(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func handle(client net.Conn) {
	defer func() { _ = client.Close() }()
	if err := client.SetReadDeadline(time.Now().Add(bannerTimeout)); err != nil {
		return
	}
	reader := bufio.NewReaderSize(client, 1024)
	banner, err := reader.ReadString('\n')
	if err != nil || len(banner) > 255 || !strings.HasPrefix(banner, "SSH-2.0-") {
		return
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	upstream, err := wakeAndConnect(context.Background())
	if err != nil {
		log.Printf("connect Bleephub SSH service: %v", err)
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := io.WriteString(upstream, banner); err != nil {
		return
	}
	idleClient := &idleDeadlineConn{Conn: client, timeout: connectionIdleTimeout}
	idleUpstream := &idleDeadlineConn{Conn: upstream, timeout: connectionIdleTimeout}
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(idleUpstream, &idleDeadlineReader{
			reader:  reader,
			conn:    client,
			timeout: connectionIdleTimeout,
		})
		_ = upstream.Close()
		close(copyDone)
	}()
	_, _ = io.Copy(idleClient, idleUpstream)
	_ = client.Close()
	<-copyDone
}

type idleDeadlineReader struct {
	reader  io.Reader
	conn    net.Conn
	timeout time.Duration
}

func (r *idleDeadlineReader) Read(p []byte) (int, error) {
	if err := r.conn.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// idleDeadlineConn refreshes the transport deadline on actual activity. A
// fixed deadline would terminate a healthy large clone, while no deadline lets
// an authenticated but idle peer occupy one of the bounded gateway slots
// forever.
type idleDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleDeadlineConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *idleDeadlineConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func wakeAndConnect(ctx context.Context) (net.Conn, error) {
	wakeURL := os.Getenv("BLEEPHUB_WAKE_URL")
	service := os.Getenv("BLEEPHUB_INTERNAL_SSH_TARGET")
	if wakeURL == "" || service == "" {
		return nil, fmt.Errorf("BLEEPHUB_WAKE_URL and BLEEPHUB_INTERNAL_SSH_TARGET are required")
	}
	parsedWakeURL, err := url.Parse(wakeURL)
	if err != nil || parsedWakeURL.Host == "" ||
		(parsedWakeURL.Scheme != "http" && parsedWakeURL.Scheme != "https") ||
		parsedWakeURL.User != nil {
		return nil, fmt.Errorf("BLEEPHUB_WAKE_URL must be an HTTP(S) URL without user information")
	}
	if strings.ContainsAny(service, "/:@?#") {
		return nil, fmt.Errorf("BLEEPHUB_INTERNAL_SSH_TARGET must be a DNS service name")
	}

	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		// #nosec G704 -- wakeURL is deployment-controlled and validated above.
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, wakeURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create bleephub wake request: %w", err)
		}
		// #nosec G704 -- the URL is deployment-controlled configuration,
		// validated above, and redirects are refused.
		response, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		for _, target := range sshTargetsFromSRV(service) {
			// #nosec G704 -- targets come only from the validated, operator-set
			// private Cloud Map SRV record and the fixed SSH port.
			connection, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err == nil {
				return connection, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("bleephub SSH service did not become reachable within %s", startupTimeout)
}

// sshTargetsFromSRV resolves the Amazon Cloud Map SRV record that Amazon ECS
// registers for the application task. The SRV record advertises HTTP on 5555,
// while the same task's SSH transport listens on 2222, so this deliberately
// preserves the registered task hostname and replaces only the port.
func sshTargetsFromSRV(service string) []string {
	// #nosec G704 -- service is validated operator configuration, not request input.
	_, records, err := net.LookupSRV("", "", service)
	if err != nil {
		return nil
	}
	return sshTargetsFromRecords(records)
}

func sshTargetsFromRecords(records []*net.SRV) []string {
	const sshPort = 2222
	targets := make([]string, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		host := strings.TrimSuffix(record.Target, ".")
		if host == "" {
			continue
		}
		target := net.JoinHostPort(host, strconv.Itoa(sshPort))
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
}

func valueOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
