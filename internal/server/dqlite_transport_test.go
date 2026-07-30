package bleephub

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/dqliteaddr"
)

// dqliteUpgradeServer runs a listener that answers one /dqlite request with the
// caller's chosen status line and Upgrade token, then echoes whatever the
// client writes. It reports the request it saw so the transport's headers can
// be asserted rather than assumed.
type dqliteUpgradeServer struct {
	address  string
	requests chan *http.Request
}

func newDqliteUpgradeServer(t *testing.T, statusLine, upgradeToken string) *dqliteUpgradeServer {
	t.Helper()
	tlsConfig, err := dqliteaddr.TLSConfig("cluster-secret", true)
	if err != nil {
		t.Fatalf("TLS config: %v", err)
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := tls.NewListener(tcpListener, tlsConfig)
	t.Cleanup(func() { _ = listener.Close() })

	server := &dqliteUpgradeServer{
		address:  listener.Addr().String(),
		requests: make(chan *http.Request, 4),
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				server.requests <- request
				response := statusLine + "\r\n"
				if upgradeToken != "" {
					response += "Connection: Upgrade\r\nUpgrade: " + upgradeToken + "\r\n"
				}
				response += "Content-Length: 0\r\n\r\n"
				if _, err := conn.Write([]byte(response)); err != nil {
					return
				}
				_, _ = io.Copy(conn, reader)
			}()
		}
	}()
	return server
}

func (s *dqliteUpgradeServer) request(t *testing.T) *http.Request {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("the dqlite transport never issued an upgrade request")
		return nil
	}
}

// TestDqliteHTTPDialUpgrades covers the production transport that carries every
// dqlite statement: it must present the cluster secret, ask for the dqlite
// upgrade, and hand back a connection that speaks to the member afterwards.
func TestDqliteHTTPDialUpgrades(t *testing.T) {
	server := newDqliteUpgradeServer(t, "HTTP/1.1 101 Switching Protocols", "dqlite")

	conn, err := dqliteHTTPDial(context.Background(), server.address, "cluster-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	request := server.request(t)
	if got := request.Header.Get(dqliteaddr.SecretHeader); got != "cluster-secret" {
		t.Fatalf("%s = %q, want the cluster secret", dqliteaddr.SecretHeader, got)
	}
	if got := request.Header.Get("Upgrade"); !strings.EqualFold(got, "dqlite") {
		t.Fatalf("Upgrade = %q, want dqlite", got)
	}
	if request.URL.Path != "/dqlite" {
		t.Fatalf("upgrade path = %q, want /dqlite", request.URL.Path)
	}

	// The connection must survive the upgrade: dqlite writes its wire protocol
	// straight onto it with no further HTTP framing.
	if _, err := conn.Write([]byte("dqlite-frame\n")); err != nil {
		t.Fatalf("write after upgrade: %v", err)
	}
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-time.After(5 * time.Second):
			_ = conn.Close()
		case <-readDone:
		}
	}()
	echoed, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}
	if echoed != "dqlite-frame\n" {
		t.Fatalf("upgraded connection carried %q", echoed)
	}
}

// TestDqliteHTTPDialRejectsUnupgradedResponses pins the failure side. An
// endpoint that answers with ordinary HTTP is not a dqlite member, and handing
// that connection to the driver would put HTTP bytes into the wire protocol.
func TestDqliteHTTPDialRejectsUnupgradedResponses(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		statusLine   string
		upgradeToken string
	}{
		{"plain 200", "HTTP/1.1 200 OK", ""},
		{"401 from a wrong secret", "HTTP/1.1 401 Unauthorized", ""},
		{"101 without the dqlite token", "HTTP/1.1 101 Switching Protocols", "websocket"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := newDqliteUpgradeServer(t, testCase.statusLine, testCase.upgradeToken)
			conn, err := dqliteHTTPDial(context.Background(), server.address, "cluster-secret")
			if err == nil {
				_ = conn.Close()
				t.Fatal("dial accepted a response that never upgraded to dqlite")
			}
			if !strings.Contains(err.Error(), "did not accept protocol upgrade") {
				t.Fatalf("error does not name the refused upgrade: %v", err)
			}
		})
	}
}

func TestDqliteHTTPDialReportsUnreachableMembers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	conn, err := dqliteHTTPDial(context.Background(), address, "cluster-secret")
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial reported success against a closed port")
	}
	if !strings.Contains(err.Error(), "dial dqlite") {
		t.Fatalf("error does not name the failed dial: %v", err)
	}
}

func TestDqliteHTTPDialRejectsAnotherClusterTLSIdentity(t *testing.T) {
	server := newDqliteUpgradeServer(t, "HTTP/1.1 101 Switching Protocols", "dqlite")
	conn, err := dqliteHTTPDial(context.Background(), server.address, "different-cluster-secret")
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial authenticated a dqlite endpoint derived from another cluster secret")
	}
	if !strings.Contains(err.Error(), "authenticate dqlite TLS endpoint") {
		t.Fatalf("wrong-cluster error = %v, want TLS authentication failure", err)
	}
}

// TestOpenDqliteRefusesIncompleteConfiguration covers the checks openDqlite
// makes before it opens a connection. Each of these silently degrades the
// cluster if it is skipped, so each must be a named error rather than a
// default.
func TestOpenDqliteRefusesIncompleteConfiguration(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		addressMap string
		secret     string
		addresses  string
		wantSubstr string
	}{
		{
			name:       "malformed address map",
			addressMap: "not-a-mapping",
			secret:     "cluster-secret",
			addresses:  "10.0.0.1:9000",
			wantSubstr: "old-address=new-address",
		},
		{
			name:       "missing cluster secret",
			secret:     "",
			addresses:  "10.0.0.1:9000",
			wantSubstr: dqliteaddr.SecretEnvironment,
		},
		{
			name:       "blank cluster secret",
			secret:     "   ",
			addresses:  "10.0.0.1:9000",
			wantSubstr: dqliteaddr.SecretEnvironment,
		},
		{
			name:       "no member addresses",
			secret:     "cluster-secret",
			addresses:  " , ,",
			wantSubstr: "at least one dqlite server address",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(dqliteaddr.Environment, testCase.addressMap)
			t.Setenv(dqliteaddr.SecretEnvironment, testCase.secret)

			db, err := openDqlite(testCase.addresses)
			if err == nil {
				_ = db.Close()
				t.Fatal("openDqlite accepted an incomplete configuration")
			}
			if !strings.Contains(err.Error(), testCase.wantSubstr) {
				t.Fatalf("error %v does not mention %q", err, testCase.wantSubstr)
			}
		})
	}
}

// TestDqliteDialerResolvesDurableAddresses proves the address map is applied
// at the driver's transport boundary. A dqlite member identity is durable in
// the cluster's own state while its network coordinate moves, so a connection
// that ignored the map would reach the retired address and never recover.
func TestDqliteDialerResolvesDurableAddresses(t *testing.T) {
	server := newDqliteUpgradeServer(t, "HTTP/1.1 101 Switching Protocols", "dqlite")
	const durable = "legacy-dqlite.invalid:9000"

	addresses, err := dqliteaddr.FromEnvironment(durable + "=" + server.address)
	if err != nil {
		t.Fatalf("parse address map: %v", err)
	}
	conn, err := dqliteDialer(addresses, "cluster-secret")(context.Background(), durable)
	if err != nil {
		t.Fatalf("dial mapped address: %v", err)
	}
	defer func() { _ = conn.Close() }()

	request := server.request(t)
	if request.Host != server.address {
		t.Fatalf("transport dialled %q, want the mapped address %q", request.Host, server.address)
	}
	if got := request.Header.Get(dqliteaddr.SecretHeader); got != "cluster-secret" {
		t.Fatalf("%s = %q on the mapped dial", dqliteaddr.SecretHeader, got)
	}
}

// TestDqliteAddressMapRejectsAmbiguity guards the parse that feeds the dial
// above: a duplicated durable identity has no single correct destination.
func TestDqliteAddressMapRejectsAmbiguity(t *testing.T) {
	_, err := dqliteaddr.FromEnvironment("a:9000=b:9000,a:9000=c:9000")
	if err == nil {
		t.Fatal("a repeated durable address was accepted")
	}
	if !strings.Contains(err.Error(), "repeats durable address") {
		t.Fatalf("error does not name the ambiguity: %v", err)
	}
	var pathError *net.OpError
	if errors.As(err, &pathError) {
		t.Fatalf("configuration parse reported a network error: %v", err)
	}
}
