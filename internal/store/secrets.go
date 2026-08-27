package store

import "time"

// EnvScopeKey packs (repoKey, envName) into one collision-free key for
// Store.EnvSecrets / Store.EnvVariables. The unit separator (not NUL: these
// are also persistence bucket keys and must stay text-safe) cannot appear in
// either half.
func EnvScopeKey(repoKey, envName string) string {
	return repoKey + "\x1f" + envName
}

// Secret is an Actions secret at repository or environment scope (OrgSecret
// embeds it for org scope).
//
// Value is persisted (workflow runs need the plaintext after a restart) but
// never marshaled to clients: the handlers emit only name/created_at/updated_at,
// matching GitHub's never-return-the-value contract. Clients send the value as
// a libsodium sealed box, which the server opens once at PUT time.
type Secret struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Value     string    `json:"value"`
}
