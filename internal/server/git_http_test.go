package bleephub

import (
	"io"
	"net/http"
	"testing"
)

// TestGitHTTPPrivateRepoExistenceOracle pins AUTH-023. The git HTTP protocol
// requires a 401 challenge for anonymous requests so the client retries with
// credentials; once authenticated, a caller who still cannot read a private
// repository gets 404 — indistinguishable from a nonexistent one.
func TestGitHTTPPrivateRepoExistenceOracle(t *testing.T) {
	const owner = "admin"

	// Set up: a private repo the caller has no credential for, and a public
	// repo so the push-needs-auth path is still reachable.
	for _, body := range []map[string]interface{}{
		{"name": "auth023-private", "private": true},
		{"name": "auth023-public", "private": false},
	} {
		resp := ghPost(t, "/api/v3/user/repos", defaultToken, body)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	cases := []struct {
		name string
		url  string
		want int
	}{
		{
			name: "nonexistent upload-pack",
			url:  "/" + owner + "/auth023-missing.git/info/refs?service=git-upload-pack",
			want: http.StatusNotFound,
		},
		{
			name: "nonexistent receive-pack",
			url:  "/" + owner + "/auth023-missing.git/info/refs?service=git-receive-pack",
			want: http.StatusNotFound,
		},
		{
			name: "private anonymous upload-pack challenges",
			url:  "/" + owner + "/auth023-private.git/info/refs?service=git-upload-pack",
			want: http.StatusUnauthorized,
		},
		{
			name: "private anonymous receive-pack challenges",
			url:  "/" + owner + "/auth023-private.git/info/refs?service=git-receive-pack",
			want: http.StatusUnauthorized,
		},
		{
			name: "public anonymous push challenges",
			url:  "/" + owner + "/auth023-public.git/info/refs?service=git-receive-pack",
			want: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(testBaseURL + tc.url)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	// Control: the repo's owner, authenticated, still reads the private repo.
	resp := ghGet(t, "/"+owner+"/auth023-private.git/info/refs?service=git-upload-pack", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated owner fetch of private repo = %d, want 200", resp.StatusCode)
	}
}
