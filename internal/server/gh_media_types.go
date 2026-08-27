package bleephub

import (
	"net/http"
	"strings"
)

// GitHub's custom media types, spelled application/vnd.github[.vN][.param][+format],
// and the X-GitHub-Media-Type header reporting which one a response was served
// with.

// acceptsGitHubMediaType reports whether Accept asks for the named GitHub
// custom media type. Both the versioned (vnd.github.v3.diff, sent by `gh`) and
// unversioned (vnd.github.diff, sent by octokit) spellings must match — GitHub
// honours both, so matching one silently ignores real clients.
func acceptsGitHubMediaType(accept, param string) bool {
	return strings.Contains(accept, "application/vnd.github."+param) ||
		strings.Contains(accept, "application/vnd.github.v3."+param)
}

// setGitHubMediaType records which custom media type the response was served
// with, so a client that branches on the header (as octokit does) does not
// treat a raw file or diff as the middleware's default JSON.
func setGitHubMediaType(w http.ResponseWriter, r *http.Request, param string) {
	value := "github.v3; param=" + param
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/vnd.github."+param+"+json") ||
		strings.Contains(accept, "application/vnd.github.v3."+param+"+json") {
		value += "; format=json"
	}
	w.Header().Set("X-GitHub-Media-Type", value)
}
