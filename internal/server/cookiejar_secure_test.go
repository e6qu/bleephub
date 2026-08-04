package bleephub

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
)

// permissiveTestJar round-trips Secure cookies over plain http, matching how
// browsers treat http://localhost and http://127.0.0.1 as secure contexts.
// Production cookies are always Secure (see identity.go); the http-based
// integration harness needs this because Go's stdlib cookie jar, unlike a
// browser, only returns Secure cookies to https URLs and would otherwise drop
// the session across requests.
type permissiveTestJar struct{ inner *cookiejar.Jar }

func newPermissiveTestJar() *permissiveTestJar {
	jar, _ := cookiejar.New(nil)
	return &permissiveTestJar{inner: jar}
}

// asSecure returns a copy of u with the https scheme so the wrapped stdlib jar
// stores and returns Secure cookies for it; only the scheme changes, so the
// host/path keying is unaffected.
func asSecure(u *url.URL) *url.URL {
	clone := *u
	clone.Scheme = "https"
	return &clone
}

func (j *permissiveTestJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.inner.SetCookies(asSecure(u), cookies)
}

func (j *permissiveTestJar) Cookies(u *url.URL) []*http.Cookie {
	return j.inner.Cookies(asSecure(u))
}
