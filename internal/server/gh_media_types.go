package bleephub

import (
	"net/http"
	"strings"
)

// GitHub's custom media types and the X-GitHub-Media-Type header that reports
// which one a response was served with.
//
// A GitHub media type is spelled application/vnd.github[.vN][.param][+format]:
// the version segment is optional and legacy, the param names the
// representation (raw, html, object, diff, patch, full…) and the +format suffix
// names the serialization. The response header echoes the parsed pieces —
// "github.v3; param=raw", or "github.v3; param=raw; format=json" when the
// client used the +json spelling — which is how a client tells a server that
// honoured its request from one that ignored it and answered with JSON anyway.

// acceptsGitHubMediaType reports whether Accept asks for the named GitHub
// custom media type.
//
// Both spellings must match: `gh` sends the versioned
// application/vnd.github.v3.diff, while octokit and the current documentation
// send application/vnd.github.diff and application/vnd.github.raw+json. GitHub
// honours all of them, so matching only one silently ignores real clients —
// which is what made `gh pr diff` print JSON.
func acceptsGitHubMediaType(accept, param string) bool {
	return strings.Contains(accept, "application/vnd.github."+param) ||
		strings.Contains(accept, "application/vnd.github.v3."+param)
}

// setGitHubMediaType records on the response which custom media type it was
// served with. Without it the middleware's default (format=json) claims a JSON
// body for a raw file or a diff, and a client that branches on the header — as
// octokit does — mis-handles the payload.
func setGitHubMediaType(w http.ResponseWriter, r *http.Request, param string) {
	value := "github.v3; param=" + param
	accept := r.Header.Get("Accept")
	// The +json suffix is a distinct request the header reports separately;
	// application/vnd.github.raw carries no format at all.
	if strings.Contains(accept, "application/vnd.github."+param+"+json") ||
		strings.Contains(accept, "application/vnd.github.v3."+param+"+json") {
		value += "; format=json"
	}
	w.Header().Set("X-GitHub-Media-Type", value)
}
