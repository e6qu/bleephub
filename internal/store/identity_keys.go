package store

import "strings"

// ExternalIdentityKey keys a federated identity by the stable (issuer,
// subject) pair, never the mutable username. An empty issuer or subject
// collapses to "" so the caller falls back to the username index rather than
// keying on half a pair.
func ExternalIdentityKey(issuer, subject string) string {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return ""
	}
	return issuer + "\x00" + subject
}
