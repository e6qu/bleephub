package store

import "time"

// Secret represents an Actions secret at any scope (repository or
// environment; OrgSecret embeds it for the organization scope).
//
// Value carries a real json name so persistence round-trips it (workflow
// runs need the plaintext after a restart). Client responses never marshal
// this struct — the secrets handlers emit name/created_at/updated_at maps,
// matching real GitHub's never-return-the-value contract. On the wire the
// value only ever arrives as a libsodium sealed box ({encrypted_value,
// key_id} against the key from the public-key endpoint); the server opens
// the box once at PUT time and stores the plaintext for job injection.
type Secret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Value     string    `json:"value"`
}
