package store

// GitHub token prefixes. Each prefix selects a different lookup table and
// auth shape in authenticateRequest; using the named constants keeps the
// middleware, stores and handlers agreeing on the exact prefix bytes.
const (
	// #nosec G101 -- public token type prefix, not a credential.
	TokenPrefixInstallation = "ghs_"
	TokenPrefixOAuthUser    = "gho_" // classic OAuth-App user token
	TokenPrefixAppUser      = "ghu_" // GitHub-App user-to-server token
	TokenPrefixRefresh      = "ghr_" // refresh token (never valid as auth)
)
