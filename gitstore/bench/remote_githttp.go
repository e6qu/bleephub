package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// backendTimeout bounds one git-http-backend run.
const backendTimeout = 10 * time.Minute

func init() {
	registerRemoteDriver("git-http-backend", func() RemoteDriver { return &gitHTTPBackend{} })
}

// gitHTTPBackend is git's own smart-HTTP server: git-http-backend, the CGI
// program git ships, over a bare repository on local disk. It is the reference
// implementation of the protocol every driver here speaks — no forge, no
// database, no hooks — so it prices the protocol itself, where git-local prices
// the filesystem with no server at all.
type gitHTTPBackend struct {
	env          Env
	root         string
	server       *http.Server
	listener     net.Listener
	created      map[string]bool
	repositories map[string]string
}

func (d *gitHTTPBackend) Name() string     { return "git-http-backend" }
func (d *gitHTTPBackend) Stores() []string { return nil }
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
	d.env, d.created, d.repositories = env, map[string]bool{}, map[string]string{}
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
		d.repositories[name] = name
	}
	return fmt.Sprintf("http://%s/%s", d.listener.Addr().String(), name), nil
}

// serve runs git-http-backend for one request, as the CGI specification says
// to. Nothing of the request reaches the child's environment: the repository
// is one this driver created, the path's remainder and the query are matched
// against the fixed set the smart protocol uses, and what does not match is
// refused. A benchmark harness needs no more of a request than that, and
// building the environment out of constants is what makes it safe to hand to
// a program rather than a sanitizer that has to be trusted.
func (d *gitHTTPBackend) serve(w http.ResponseWriter, r *http.Request) {
	repository, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || !d.created[repository] {
		http.NotFound(w, r)
		return
	}
	suffix, known := gitServicePaths[rest]
	if !known {
		http.NotFound(w, r)
		return
	}
	query, known := gitServiceQueries[r.URL.RawQuery]
	if !known {
		http.NotFound(w, r)
		return
	}
	protocol, known := gitProtocols[r.Header.Get("Git-Protocol")]
	if !known {
		http.Error(w, "unexpected git protocol", http.StatusBadRequest)
		return
	}
	contentType, known := gitContentTypes[r.Header.Get("Content-Type")]
	if !known {
		http.Error(w, "unexpected content type", http.StatusUnsupportedMediaType)
		return
	}
	method := http.MethodGet
	if r.Method == http.MethodPost {
		method = http.MethodPost
	}

	// The client sends a push chunked, so its length is not known from the
	// header; CGI requires one, so the body is read before the child starts.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The child's deadline is the harness's own, not the request's: gosec reads
	// anything of the request reaching the call as command injection (G702),
	// and a scenario that is being measured should not be cut short by a client
	// that hung up either.
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	// #nosec G204 -- git is a fixed executable found on PATH, given no argument
	// from the request.
	cmd := exec.CommandContext(ctx, gitBinary(), "http-backend")
	cmd.Dir = d.root
	cmd.Env = []string{
		"GIT_PROJECT_ROOT=" + d.root,
		"GIT_HTTP_EXPORT_ALL=1",
		"REMOTE_USER=bench",
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL=HTTP/1.1",
		"REQUEST_METHOD=" + method,
		"PATH_INFO=/" + d.repositories[repository] + suffix,
		"QUERY_STRING=" + query,
		"CONTENT_TYPE=" + contentType,
		"CONTENT_LENGTH=" + strconv.Itoa(len(body)),
		"GIT_PROTOCOL=" + protocol,
	}
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "git-http-backend: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The child answers with CGI headers, a blank line, then the body.
	head, answer, found := bytes.Cut(out, []byte("\r\n\r\n"))
	if !found {
		head, answer, found = bytes.Cut(out, []byte("\n\n"))
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
	_, _ = w.Write(answer)
}

// What the smart protocol asks for, and nothing else. Each is the constant the
// child is given, keyed by what the client sent.
var (
	gitServicePaths = map[string]string{
		"info/refs":        "/info/refs",
		"git-upload-pack":  "/git-upload-pack",
		"git-receive-pack": "/git-receive-pack",
		"HEAD":             "/HEAD",
	}
	gitServiceQueries = map[string]string{
		"":                         "",
		"service=git-upload-pack":  "service=git-upload-pack",
		"service=git-receive-pack": "service=git-receive-pack",
	}
	// The client asks for a protocol version; it is passed on only as it came,
	// and forcing version 2 broke the push advertisement.
	gitProtocols = map[string]string{
		"":          "",
		"version=2": "version=2",
	}
	gitContentTypes = map[string]string{
		"":                                       "",
		"application/x-git-upload-pack-request":  "application/x-git-upload-pack-request",
		"application/x-git-receive-pack-request": "application/x-git-receive-pack-request",
	}
)

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
