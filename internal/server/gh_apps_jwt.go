package bleephub

import (
	"strings"
)

func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}
