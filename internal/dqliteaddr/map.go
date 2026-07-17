// Package dqliteaddr resolves durable dqlite member identities to their live
// network coordinates.
package dqliteaddr

import (
	"fmt"
	"strings"
)

const Environment = "BLEEPHUB_DQLITE_ADDRESS_MAP"

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
