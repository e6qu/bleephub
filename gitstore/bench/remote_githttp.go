package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerRemoteDriver("git-http-backend", func() RemoteDriver { return &gitHTTPBackend{} })
}

// gitHTTPBackend is git's own smart-HTTP server: git-http-backend, the CGI
// program git ships, over a bare repository on local disk. It is the reference
// implementation of the protocol every driver here speaks — no forge, no
// database, no hooks — so it prices the protocol itself, where git-local prices
// the filesystem with no server at all.
type gitHTTPBackend struct {
	env      Env
	root     string
	server   *http.Server
	listener net.Listener
	created  map[string]bool
}

func (d *gitHTTPBackend) Name() string { return "git-http-backend" }
func (d *gitHTTPBackend) Describe() string {
	return "git's own smart-HTTP server (git-http-backend over a bare repository): the protocol with no forge on top"
}

func (d *gitHTTPBackend) Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return err
	}
	return nil
}

func (d *gitHTTPBackend) Setup(_ context.Context, env Env) error {
	d.env, d.created = env, map[string]bool{}
	root, err := env.tempDir("git-http-backend-*")
	if err != nil {
		return err
	}
	d.root = root

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	d.listener = listener
	// git-http-backend is a CGI program, and it is run as one here rather than
	// through net/http/cgi: that package passes the request's headers on as
	// environment variables, which is the Httpoxy hole (CVE-2016-5386), and
	// nothing in this benchmark needs a request to reach the environment. The
	// child is given exactly the variables below, and the request's bytes on
	// its standard input.
	d.server = &http.Server{Handler: http.HandlerFunc(d.serve), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = d.server.Serve(listener) }()
	return nil
}

// Remote makes the bare repository the first push needs; git-http-backend
// serves repositories, it does not create them.
func (d *gitHTTPBackend) Remote(ctx context.Context, repo string) (string, error) {
	name := strings.ReplaceAll(repo, "/", "-") + ".git"
	if !d.created[name] {
		path := filepath.Join(d.root, name)
		if err := os.MkdirAll(path, 0o750); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, d.root, nil, nil, "init", "--bare", "--quiet", path); err != nil {
			return "", err
		}
		// http-backend refuses a push to a repository that has not asked for it.
		if _, err := runGit(ctx, path, nil, nil, "config", "http.receivepack", "true"); err != nil {
			return "", err
		}
		d.created[name] = true
	}
	return fmt.Sprintf("http://%s/%s", d.listener.Addr().String(), name), nil
}

// serve runs git-http-backend for one request, as the CGI specification says
// to, with an environment this function writes in full.
func (d *gitHTTPBackend) serve(w http.ResponseWriter, r *http.Request) {
	// #nosec G204 -- git is a fixed executable found on PATH, given no argument
	// from the request.
	cmd := exec.CommandContext(r.Context(), gitBinary(), "http-backend")
	cmd.Dir = d.root
	cmd.Env = []string{
		"GIT_PROJECT_ROOT=" + d.root,
		"GIT_HTTP_EXPORT_ALL=1",
		"REMOTE_USER=bench",
		"REQUEST_METHOD=" + r.Method,
		"PATH_INFO=" + r.URL.Path,
		"QUERY_STRING=" + r.URL.RawQuery,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"CONTENT_LENGTH=" + r.Header.Get("Content-Length"),
		"HTTP_CONTENT_ENCODING=" + r.Header.Get("Content-Encoding"),
		"GIT_PROTOCOL=" + r.Header.Get("Git-Protocol"),
		"SERVER_PROTOCOL=" + r.Proto,
		"GATEWAY_INTERFACE=CGI/1.1",
	}
	cmd.Stdin = r.Body
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "git-http-backend: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The child answers with CGI headers, a blank line, then the body.
	head, body, found := bytes.Cut(out, []byte("\r\n\r\n"))
	if !found {
		head, body, found = bytes.Cut(out, []byte("\n\n"))
	}
	if !found {
		http.Error(w, "git-http-backend answered without a header block", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Status") {
			if code, err := strconv.Atoi(strings.Fields(value)[0]); err == nil {
				status = code
			}
			continue
		}
		w.Header().Add(name, value)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (d *gitHTTPBackend) GitEnv() []string { return nil }

// Restart is the server's, not the repositories': they are files, and a cold
// replica of a filesystem server is a fresh process over the same disk.
func (d *gitHTTPBackend) Restart(ctx context.Context) error { return nil }

func (d *gitHTTPBackend) Close() error {
	if d.server == nil {
		return nil
	}
	return d.server.Close()
}

func gitBinary() string {
	path, err := exec.LookPath("git")
	if err != nil {
		return "git"
	}
	return path
}
