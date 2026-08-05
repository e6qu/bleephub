// Package dqliteaddr resolves durable dqlite member identities to their live
// network coordinates.
package dqliteaddr

import (
	"fmt"
	"net"
	"strings"
)

const Environment = "BLEEPHUB_DQLITE_ADDRESS_MAP"

// SecretEnvironment names the shared cluster credential and SecretHeader the
// request header that carries it. The dqlite wire protocol authenticates
// nothing itself, so the transport upgrade is where membership is proven.
const (
	SecretEnvironment = "BLEEPHUB_DQLITE_SECRET"   // #nosec G101 -- environment variable name
	SecretHeader      = "X-Bleephub-Dqlite-Secret" // #nosec G101 -- header name, not a credential
)

// Map parses the comma-separated old-address=new-address mapping stored in
// Environment. Member identities remain durable in dqlite's state while the
// destination can move from a retired transport to a stable private address.
type Map map[string]string

// FromEnvironment parses an address map. Empty configuration is valid.
func FromEnvironment(value string) (Map, error) {
	mapping := Map{}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		oldAddress, newAddress, ok := strings.Cut(entry, "=")
		oldAddress = strings.TrimSpace(oldAddress)
		newAddress = strings.TrimSpace(newAddress)
		if !ok || oldAddress == "" || newAddress == "" {
			return nil, fmt.Errorf("%s entry %q must be old-address=new-address", Environment, entry)
		}
		if _, exists := mapping[oldAddress]; exists {
			return nil, fmt.Errorf("%s repeats durable address %q", Environment, oldAddress)
		}
		mapping[oldAddress] = newAddress
	}
	return mapping, nil
}

// Resolve returns the live coordinate for a durable dqlite member identity.
func (m Map) Resolve(address string) string {
	if replacement, ok := m[address]; ok {
		return replacement
	}
	return address
}

// ListenAddr resolves the TCP address a dqlite node should listen on. An
// explicit override (listenEnv, from BLEEPHUB_DQLITE_LISTEN_ADDR) wins so a
// node behind NAT or a proxy can bind a different local address. Otherwise the
// node listens on the very port it advertises to peers: defaulting to a fixed
// ":9000" while the advertised address used a different port left peers dialing
// the advertised port and failing to connect (STORE-055).
func ListenAddr(listenEnv, advertise string) (string, error) {
	if strings.TrimSpace(listenEnv) != "" {
		return strings.TrimSpace(listenEnv), nil
	}
	_, port, err := net.SplitHostPort(advertise)
	if err != nil {
		return "", fmt.Errorf("derive listen address from advertise address %q: %w", advertise, err)
	}
	return ":" + port, nil
}
