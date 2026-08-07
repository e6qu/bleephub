package bleephub

import (
	"io"
	"net/http"
	"testing"
)

// TestGitHTTPInvalidCredentialIs401 pins that a presented-but-invalid
// credential on the git-HTTP surface earns a 401, not a silent downgrade to an
// anonymous 200 on a public repo.
func TestGitHTTPInvalidCredentialIs401(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	srv.post(t, "/api/v3/user/repos", defaultToken, map[string]interface{}{"name": "auth114-public", "private": false})

	req, err := http.NewRequest("GET", srv.baseURL+"/admin/auth114-public.git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token ghp_revoked-not-a-real-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid-credential fetch = %d, want 401", resp.StatusCode)
	}
}

// TestGitHTTPPrivateRepoExistenceOracle guards the existence oracle. The git HTTP protocol
// requires a 401 challenge for anonymous requests so the client retries with
// credentials — and the challenge is issued whether or not the repository
// exists, so an anonymous prober learns nothing from the status code. Once
// authenticated, a caller who still cannot read a private repository gets
// 404 — indistinguishable from a nonexistent one.
func TestGitHTTPPrivateRepoExistenceOracle(t *testing.T) {
	t.Parallel()
	srv := newIsolatedServer(t)
	const owner = "admin"

	// Set up: a private repo the caller has no credential for, and a public
	// repo so the push-needs-auth path is still reachable.
	for _, body := range []map[string]interface{}{
		{"name": "auth023-private", "private": true},
		{"name": "auth023-public", "private": false},
	} {
		resp := srv.post(t, "/api/v3/user/repos", defaultToken, body)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	cases := []struct {
		name string
		url  string
		want int
	}{
		{
			name: "nonexistent anonymous upload-pack challenges",
			url:  "/" + owner + "/auth023-missing.git/info/refs?service=git-upload-pack",
			want: http.StatusUnauthorized,
		},
		{
			name: "nonexistent anonymous receive-pack challenges",
			url:  "/" + owner + "/auth023-missing.git/info/refs?service=git-receive-pack",
			want: http.StatusUnauthorized,
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
			resp, err := http.Get(srv.baseURL + tc.url)
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

	// Once authenticated, a nonexistent repository is a plain 404.
	resp := srv.get(t, "/"+owner+"/auth023-missing.git/info/refs?service=git-upload-pack", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("authenticated fetch of nonexistent repo = %d, want 404", resp.StatusCode)
	}

	// Control: the repo's owner, authenticated, still reads the private repo.
	resp = srv.get(t, "/"+owner+"/auth023-private.git/info/refs?service=git-upload-pack", defaultToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated owner fetch of private repo = %d, want 200", resp.StatusCode)
	}
}
