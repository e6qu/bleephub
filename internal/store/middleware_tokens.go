package store

// GitHub token prefixes. Each selects a different lookup table and auth shape
// in authenticateRequest.
const (
	// #nosec G101 -- public token type prefix, not a credential.
	TokenPrefixInstallation = "ghs_"
	TokenPrefixOAuthUser    = "gho_" // classic OAuth-App user token
	TokenPrefixAppUser      = "ghu_" // GitHub-App user-to-server token
	TokenPrefixRefresh      = "ghr_" // refresh token (never valid as auth)
)
