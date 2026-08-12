package bleephub

import (
	"strings"
)

// looksLikeJWT returns true if the string has the structure of a JWT.
func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}
