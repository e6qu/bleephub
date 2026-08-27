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
	// SCP-style URLs can't encode a port, so a host:port uses the ssh:// form.
	if _, _, err := net.SplitHostPort(host); err == nil {
		return "ssh://git@" + host + "/" + fullName + ".git"
	}
	return "git@" + host + ":" + fullName + ".git"
}
