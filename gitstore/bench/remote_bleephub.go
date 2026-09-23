package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	registerRemoteDriver("bleephub", func() RemoteDriver { return &bleephubRemote{storage: objectStoreRepositories{}} })
	registerRemoteDriver("bleephub-dir", func() RemoteDriver { return &bleephubRemote{storage: directoryRepositories{}} })
}

// bleephubStorage is where a bleephub server under measurement keeps its
// repositories. The two are not degrees of one setting: a deployment holds its
// repositories in an object store or in a directory, and says which.
type bleephubStorage interface {
	// name is the driver's name, and describe what the table's row means.
	name() string
	describe() string
	// settings are the server's storage settings, given the run's scratch
	// directories. Only the chosen one's settings may be set.
	settings(d *bleephubRemote, cacheDir, repositoryDir string) []string
}

// objectStoreRepositories is gitstore as bleephub is deployed: repositories in
// the object store behind the harness's meter.
type objectStoreRepositories struct{}

func (objectStoreRepositories) name() string { return "bleephub" }
func (objectStoreRepositories) describe() string {
	return "the bleephub server on gitstore, over smart HTTP: pushes land as packs, ranged reads, pack cache, compaction"
}

func (objectStoreRepositories) settings(d *bleephubRemote, cacheDir, _ string) []string {
	// A deployment has one endpoint per driver, which serves the git store and
	// the byte store (artifacts, logs, packages) alike, so the byte store is
	// behind the meter too. What it adds to a git benchmark is the conformance
	// probe it runs when the server starts — the same dozen or so requests the
	// git store's probe makes — and nothing per git operation: no scenario here
	// uploads an artifact, a log or a package.
	return []string{
		"BLEEPHUB_OBJECT_STORE=s3",
		"BLEEPHUB_S3_ENDPOINT=" + d.env.Endpoint,
		"BLEEPHUB_S3_REGION=" + d.env.Region,
		"BLEEPHUB_GIT_BUCKET=" + d.env.Bucket,
		"BLEEPHUB_GIT_PREFIX=" + d.env.Prefix + "/bleephub",
		"BLEEPHUB_OBJECT_BUCKET=" + d.env.Bucket,
		"BLEEPHUB_OBJECT_PREFIX=" + d.env.Prefix + "/bleephub-objects",
		"BLEEPHUB_GITSTORE_CACHE_DIR=" + cacheDir,
		"AWS_ACCESS_KEY_ID=" + d.env.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + d.env.SecretKey,
		"AWS_EC2_METADATA_DISABLED=true",
	}
}

// directoryRepositories is the same server with its repositories in a local
// directory, which is what the single-binary git servers this harness is
// pointed at all do — so it is the row to compare with those, rather than with
// the object stores.
//
// Its byte store is still the run's object store. That is not a compromise in
// the measurement: a persistent bleephub refuses to start without object-backed
// byte storage, because artifacts, logs, release assets, packages and LFS
// objects live there, and no git scenario here writes any of them. What the
// object store costs this driver is the conformance probe at startup, which is
// in replica-start and in no other row; every git operation it is measured on
// reaches the directory alone.
type directoryRepositories struct{}

func (directoryRepositories) name() string { return "bleephub-dir" }
func (directoryRepositories) describe() string {
	return "the bleephub server with its repositories in a local directory, over smart HTTP: the row to compare with a filesystem git server"
}

func (directoryRepositories) settings(d *bleephubRemote, _, repositoryDir string) []string {
	return []string{
		"BLEEPHUB_GIT_DIR=" + repositoryDir,
		"BLEEPHUB_OBJECT_STORE=s3",
		"BLEEPHUB_S3_ENDPOINT=" + d.env.Endpoint,
		"BLEEPHUB_S3_REGION=" + d.env.Region,
		"BLEEPHUB_OBJECT_BUCKET=" + d.env.Bucket,
		"BLEEPHUB_OBJECT_PREFIX=" + d.env.Prefix + "/bleephub-dir-objects",
		"AWS_ACCESS_KEY_ID=" + d.env.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + d.env.SecretKey,
		"AWS_EC2_METADATA_DISABLED=true",
	}
}

// bleephubBinary is the server to run. Empty builds it from this checkout.
var bleephubBinary string

// bleephubRemote is gitstore as it is deployed: the bleephub server itself, its
// repositories in the object store, reached over smart HTTP. Running the real
// binary rather than linking the library in measures what a user of the server
// gets, and keeps the server's dependency tree out of this module.
type bleephubRemote struct {
	storage       bleephubStorage
	env           Env
	binary        string
	dataDir       string
	repositoryDir string
	token         string
	key           string
	port          int
	cmd           *exec.Cmd
	exited        chan error
	output        bytes.Buffer
	created       map[string]bool
}

func (d *bleephubRemote) Name() string     { return d.storage.name() }
func (d *bleephubRemote) Describe() string { return d.storage.describe() }

func (d *bleephubRemote) Available() error {
	if bleephubBinary != "" {
		if _, err := os.Stat(bleephubBinary); err != nil {
			return fmt.Errorf("-bleephub-bin: %w", err)
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join("..", "..", "cmd", "bleephub")); err != nil {
		return errors.New("not run from gitstore/bench of a bleephub checkout; pass -bleephub-bin")
	}
	return nil
}

func (d *bleephubRemote) Setup(ctx context.Context, env Env) error {
	d.env, d.created = env, map[string]bool{}
	dataDir, err := env.tempDir("bleephub-data-*")
	if err != nil {
		return err
	}
	d.dataDir = dataDir

	d.binary = bleephubBinary
	if d.binary == "" {
		d.binary = filepath.Join(env.TempDir, "bleephub-server")
		if _, err := os.Stat(d.binary); err != nil {
			// #nosec G204 -- go is a fixed executable; the output path is the
			// harness's own scratch directory.
			build := exec.CommandContext(ctx, "go", "build", "-tags", "noui", "-o", d.binary, "./cmd/bleephub")
			build.Dir = filepath.Join("..", "..")
			build.Env = append(os.Environ(), "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				return fmt.Errorf("build bleephub: %w\n%s", err, out)
			}
		}
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	d.key = base64.StdEncoding.EncodeToString(secret)
	d.token = "bench-" + hex.EncodeToString(secret[:20])
	return d.start(ctx)
}

// start launches the server with a pack cache of its own, so every start is a
// replica that has never served anything. The metadata database survives in
// dataDir, which is what lets a restarted server still know its repositories.
func (d *bleephubRemote) start(ctx context.Context) error {
	// The port is chosen once: a restarted server must answer at the URL the
	// client was already given, as a replaced replica does behind its address.
	if d.port == 0 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		d.port = listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
	}
	cacheDir, err := d.env.tempDir("bleephub-cache-*")
	if err != nil {
		return err
	}
	// The repository directory outlives a restart, as the object store does: a
	// cold scenario is a replica with an empty cache, not an empty server.
	if d.repositoryDir == "" {
		if d.repositoryDir, err = d.env.tempDir("bleephub-repositories-*"); err != nil {
			return err
		}
	}

	// #nosec G204 -- the binary is the server this harness built, or the one the
	// operator named with -bleephub-bin.
	d.cmd = exec.Command(d.binary, "-addr", fmt.Sprintf("127.0.0.1:%d", d.port), "-log-level", "warn")
	d.output.Reset()
	d.cmd.Stdout, d.cmd.Stderr = &d.output, &d.output
	// A deployment has one endpoint per driver, which serves the git store and
	// the byte store (artifacts, logs, packages) alike, so the byte store is
	// behind the meter too. What it adds to a git benchmark is the conformance
	// probe it runs when the server starts — the same dozen or so requests the
	// git store's probe makes — and nothing per git operation: no scenario here
	// uploads an artifact, a log or a package.
	d.cmd.Env = append(append(os.Environ(),
		"BLEEPHUB_ADMIN_TOKEN="+d.token,
		"BLEEPHUB_PERSIST=true",
		"BLEEPHUB_DATA_DIR="+d.dataDir,
		"BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY="+d.key,
	), d.storage.settings(d, cacheDir, d.repositoryDir)...)
	if err := d.cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	d.exited = exited
	go func(cmd *exec.Cmd) { exited <- cmd.Wait() }(d.cmd)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			d.cmd = nil
			return fmt.Errorf("bleephub exited during startup (%v):\n%s", err, d.output.String())
		default:
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.api("/api/v3/meta"), nil)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = d.stop()
	return fmt.Errorf("bleephub did not become ready:\n%s", d.output.String())
}

func (d *bleephubRemote) api(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", d.port, path)
}

func (d *bleephubRemote) stop() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	_ = d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-d.exited:
	case <-time.After(20 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited
	}
	d.cmd = nil
	return nil
}

func (d *bleephubRemote) Remote(ctx context.Context, repo string) (string, error) {
	_, name, _ := strings.Cut(repo, "/")
	if !d.created[name] {
		body := strings.NewReader(fmt.Sprintf(`{"name":%q}`, name))
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.api("/api/v3/user/repos"), body)
		if err != nil {
			return "", err
		}
		request.Header.Set("Authorization", "token "+d.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return "", err
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			return "", fmt.Errorf("create repository %s: HTTP %d", name, response.StatusCode)
		}
		d.created[name] = true
	}
	return fmt.Sprintf("http://admin:%s@127.0.0.1:%d/admin/%s.git", d.token, d.port, name), nil
}

func (d *bleephubRemote) GitEnv() []string { return nil }

func (d *bleephubRemote) Restart(ctx context.Context) error {
	if err := d.stop(); err != nil {
		return err
	}
	return d.start(ctx)
}

func (d *bleephubRemote) Close() error { return d.stop() }
