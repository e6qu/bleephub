package store

import (
	"net"
	"os"
	"strings"
)

func SshGitURL(fullName string) string {
	host := strings.TrimSpace(os.Getenv("BLEEPHUB_SSH_HOST"))
	if host == "" {
		return ""
	}
	// SCP-style Git URLs cannot encode a non-default SSH port. A configured
	// host-and-port therefore uses the standard SSH URL form instead.
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "ssh://git@" + host + "/" + fullName + ".git"
	}
	return "git@" + host + ":" + fullName + ".git"
}
