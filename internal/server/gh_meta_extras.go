package bleephub

import (
	cryptorand "crypto/rand"
	_ "embed"
	"encoding/json"
	"html"
	"math/big"
	"net/http"
	"regexp"
	"strings"
)

// Top-level GitHub REST API meta surfaces: emojis, zen, octocat, API versions,
// and credential revocation.

// gemojiCatalog is the github/gemoji dataset as a "name path" per line; path is
// relative to <base>/images/icons/emoji/.
//
//go:embed gemoji_catalog.txt
var gemojiCatalog string

// zenQuotes is GitHub's zen quote set, served by GET /zen and used as the
// octocat speech bubble fallback.
var zenQuotes = []string{
	"Accessible for all.",
	"Anything added dilutes everything else.",
	"Approachable is better than simple.",
	"Avoid administrative distraction.",
	"Design for failure.",
	"Encourage flow.",
	"Favor focus over features.",
	"Half measures are as bad as nothing at all.",
	"It's not fully shipped until it's fast.",
	"Keep it logically awesome.",
	"Mind your words, they are important.",
	"Non-blocking is better than blocking.",
	"Practicality beats purity.",
	"Responsive is better than fast.",
	"Speak like a human.",
}

// octocatSpeechRe is the character set GitHub accepts for the octocat `s`
// parameter; anything outside it falls back to a random zen quote.
var octocatSpeechRe = regexp.MustCompile(`^[A-Za-z0-9_ ,\-/]+$`)

func (s *Server) registerGHMetaExtrasRoutes() {
	s.route("GET /api/v3/emojis", s.handleGHEmojis)
	s.route("GET /api/v3/zen", s.handleGHZen)
	s.route("GET /api/v3/octocat", s.handleGHOctocat)
	s.route("GET /api/v3/versions", s.handleGHAPIVersions)
	s.route("POST /api/v3/credentials/revoke", s.handleGHCredentialsRevoke)
	// Instance-hosted emoji images the /emojis catalog points at; a top-level
	// GHES asset path, not part of /api/v3.
	s.route("GET /images/icons/emoji/{path...}", s.handleGHEmojiImage)
}

// handleGHEmojis serves the emoji catalog with image URLs pointing at this
// server, as GHES serves emoji assets from the instance host.
func (s *Server) handleGHEmojis(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r) + "/images/icons/emoji/"
	out := make(map[string]string, 2048)
	for _, line := range strings.Split(strings.TrimSpace(gemojiCatalog), "\n") {
		name, path, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		out[name] = base + path
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGHZen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(randomZenQuote()))
}

func (s *Server) handleGHOctocat(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("s")
	if !octocatSpeechRe.MatchString(text) {
		text = randomZenQuote()
	} else {
		// A no-op given the allowlist admits no HTML metacharacters; kept so taint
		// analysis can prove non-injectability without reading the regexp.
		text = html.EscapeString(text)
	}
	w.Header().Set("Content-Type", "application/octocat-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(octocatArt(text)))
}

func randomZenQuote() string {
	index, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(zenQuotes))))
	if err != nil {
		return zenQuotes[0]
	}
	return zenQuotes[index.Int64()]
}

// octocatArt renders the octocat ASCII art with text in the speech bubble,
// byte-identical to GET https://api.github.com/octocat.
func octocatArt(text string) string {
	inner := len(text) + 2
	bottomTail := inner - 4
	if bottomTail < 0 {
		bottomTail = 0
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("               MMM.           .MMM\n")
	b.WriteString("               MMMMMMMMMMMMMMMMMMM\n")
	b.WriteString("               MMMMMMMMMMMMMMMMMMM      " + strings.Repeat("_", inner) + "\n")
	b.WriteString("              MMMMMMMMMMMMMMMMMMMMM    |" + strings.Repeat(" ", inner) + "|\n")
	b.WriteString("             MMMMMMMMMMMMMMMMMMMMMMM   | " + text + " |\n")
	b.WriteString("            MMMMMMMMMMMMMMMMMMMMMMMM   |_   " + strings.Repeat("_", bottomTail) + "|\n")
	b.WriteString("            MMMM::- -:::::::- -::MMMM    |/\n")
	b.WriteString("             MM~:~ 00~:::::~ 00~:~MM\n")
	b.WriteString("        .. MMMMM::.00:::+:::.00::MMMMM ..\n")
	b.WriteString("              .MM::::: ._. :::::MM.\n")
	b.WriteString("                 MMMM;:::::;MMMM\n")
	b.WriteString("          -MM        MMMMMMM\n")
	b.WriteString("          ^  M+     MMMMMMMMM\n")
	b.WriteString("              MMMMMMM MM MM MM\n")
	b.WriteString("                   MM MM MM MM\n")
	b.WriteString("                   MM MM MM MM\n")
	b.WriteString("                .~~MM~MM~MM~MM~~.\n")
	b.WriteString("             ~~~~MM:~MM~~~MM~:MM~~~~\n")
	b.WriteString("            ~~~~~~==~==~~~==~==~~~~~~\n")
	b.WriteString("             ~~~~~~==~==~==~==~~~~~~\n")
	b.WriteString("                 :~==~==~==~==~~\n")
	return b.String()
}

func (s *Server) handleGHAPIVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, supportedGitHubAPIVersions)
}

// handleGHCredentialsRevoke revokes every supplied credential matching a live
// token. Unknown credentials are silently accepted (answered 202, no body), as
// GitHub does, so a caller cannot probe which tokens exist.
func (s *Server) handleGHCredentialsRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Credentials []string `json:"credentials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if len(req.Credentials) < 1 || len(req.Credentials) > 1000 {
		writeGHValidationErrorSimple(w, "credentials is invalid")
		return
	}
	s.store.RevokeCredentials(req.Credentials)
	w.WriteHeader(http.StatusAccepted)
}
