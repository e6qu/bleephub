package store

import "strings"

func SplitRepoFullName(fullName string) (owner, name string, ok bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
