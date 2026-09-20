package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func init() {
	registerRemoteDriver("walgit", func() RemoteDriver { return &walgitRemote{} })
}

// walgitBinary is the walgit server to run. Empty looks it up on PATH.
var walgitBinary string

// walgitRemote is tobi/walgit: a smart-HTTP git server in Rust that keeps packs
// in the object store and orders pushes through a write-ahead log whose manifest
// it swaps with a conditional write. It is the closest design to gitstore in
// this comparison — a server, packs, a local cache — so the differences between
// the two are differences of engineering rather than of approach.
//
// It materializes a repository on local disk before serving it, which is why a
// restart here gives it a fresh cache directory: without that, the cold-clone
// scenarios would be measuring its disk.
type walgitRemote struct {
	env    Env
	binary string
	port   int
	cmd    *exec.Cmd
	exited chan error
	output bytes.Buffer
}

func (d *walgitRemote) Name() string { return "walgit" }
func (d *walgitRemote) Describe() string {
	return "tobi/walgit server over smart HTTP: packs plus a write-ahead log, manifest swapped by conditional write, repositories materialized to a local cache"
}

func (d *walgitRemote) Available() error {
	if walgitBinary != "" {
		if _, err := os.Stat(walgitBinary); err != nil {
			return fmt.Errorf("-walgit-bin: %w", err)
		}
		return nil
	}
	if _, err := exec.LookPath("walgit"); err != nil {
		return errors.New("walgit is not on PATH; build github.com/tobi/walgit and pass -walgit-bin")
	}
	return nil
}

func (d *walgitRemote) Setup(ctx context.Context, env Env) error {
	d.env = env
	d.binary = walgitBinary
	if d.binary == "" {
		binary, err := exec.LookPath("walgit")
		if err != nil {
			return err
		}
		d.binary = binary
	}
	return d.start(ctx)
}

func (d *walgitRemote) start(ctx context.Context) error {
	// The port is chosen once, for the reason the bleephub driver gives: a
	// replaced replica answers at the address its clients already hold.
	if d.port == 0 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		d.port = listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
	}
	runDir, err := d.env.tempDir("walgit-*")
	if err != nil {
		return err
	}
	config := filepath.Join(runDir, "walgit.toml")
	if err := os.WriteFile(config, []byte(d.config(filepath.Join(runDir, "cache"))), 0o600); err != nil {
		return err
	}

	// #nosec G204 -- the binary is the one the operator named with -walgit-bin
	// or installed on PATH; the config path is the harness's own scratch file.
	d.cmd = exec.Command(d.binary, "--config", config, "serve")
	d.output.Reset()
	d.cmd.Stdout, d.cmd.Stderr = &d.output, &d.output
	d.cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+d.env.AccessKey,
		"AWS_SECRET_ACCESS_KEY="+d.env.SecretKey,
		"AWS_EC2_METADATA_DISABLED=true",
	)
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
			return fmt.Errorf("walgit exited during startup (%v):\n%s", err, d.output.String())
		default:
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/readyz", d.port), nil)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = d.stop()
	return fmt.Errorf("walgit did not become ready:\n%s", d.output.String())
}

// config is the smallest configuration that serves anonymous pushes on
// loopback. Everything it leaves out keeps walgit's own default, so what is
// measured is walgit as it ships, not a tuning of it.
func (d *walgitRemote) config(cacheDir string) string {
	return fmt.Sprintf(`[server]
listen = "127.0.0.1:%d"
auto_create_on_push = true

[server.tls]
mode = "off"

[server.auth]
mode = "none"

[store]
backend = "s3"
bucket = %q
prefix = %q

[store.s3]
endpoint = %q
region = %q
force_path_style = true

[cache]
dir = %q
`, d.port, d.env.Bucket, d.env.Prefix+"/walgit", d.env.Endpoint, d.env.Region, cacheDir)
}

func (d *walgitRemote) stop() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	_ = d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-d.exited:
	case <-time.After(30 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited
	}
	d.cmd = nil
	return nil
}

func (d *walgitRemote) Remote(_ context.Context, repo string) (string, error) {
	return fmt.Sprintf("http://127.0.0.1:%d/%s.git", d.port, repo), nil
}

func (d *walgitRemote) GitEnv() []string { return nil }

func (d *walgitRemote) Restart(ctx context.Context) error {
	if err := d.stop(); err != nil {
		return err
	}
	return d.start(ctx)
}

func (d *walgitRemote) Close() error { return d.stop() }
