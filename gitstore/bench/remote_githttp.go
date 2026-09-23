package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
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
	// Pushing over the protocol is refused unless the repository says otherwise,
	// and the CGI needs the repository root and the client's identity.
	handler := &cgi.Handler{
		Path: gitBinary(),
		Args: []string{"http-backend"},
		Dir:  d.root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + d.root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=bench",
		},
	}
	d.server = &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
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
