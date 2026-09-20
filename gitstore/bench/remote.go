package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// RemoteDriver is an implementation reached the way its users reach it: through
// the stock git client, as a remote. A smart-HTTP server is one; so is a remote
// helper that git invokes for a URL scheme. Nothing is linked in, so an
// implementation in any language takes part, and what is timed is what a
// developer waits for — negotiation, transfer and the client's own work
// included. That is also why this level is not comparable with the Storer
// level's figures: each level is compared within itself.
type RemoteDriver interface {
	Name() string
	// Describe says, in a line, how the driver keeps git data.
	Describe() string
	// Available reports why the driver cannot run here, or nil. A missing
	// helper binary is a reason to skip the driver, not to fail the run.
	Available() error
	// Setup prepares the driver for one run.
	Setup(ctx context.Context, env Env) error
	// Remote returns the URL git is given for repo, creating whatever must
	// exist server-side first.
	Remote(ctx context.Context, repo string) (string, error)
	// GitEnv is added to the environment of every git invocation.
	GitEnv() []string
	// Restart drops everything the implementation holds outside the object
	// store, as a replica that has never served the repository would start.
	// A helper that keeps nothing between invocations has nothing to drop.
	Restart(ctx context.Context) error
	Close() error
}

var remoteDrivers = map[string]func() RemoteDriver{}

func registerRemoteDriver(name string, build func() RemoteDriver) {
	remoteDrivers[name] = build
}

func remoteDriverNames() []string {
	names := make([]string, 0, len(remoteDrivers))
	for name := range remoteDrivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newRemoteDriver(name string) (RemoteDriver, error) {
	build, ok := remoteDrivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown git-level driver %q (have %v)", name, remoteDriverNames())
	}
	return build(), nil
}

func init() {
	registerRemoteDriver("git-local", func() RemoteDriver { return &localRemote{} })
	registerRemoteDriver("git-remote-s3", func() RemoteDriver {
		return &helperRemote{
			name:     "git-remote-s3",
			describe: "awslabs/git-remote-s3 helper: one full bundle per ref per push",
			binary:   "git-remote-s3",
			template: "s3://{bucket}/{prefix}/git-remote-s3/{repo}",
			pathConf: true,
		}
	})
	registerRemoteDriver("git-remote-object-store", func() RemoteDriver {
		return &helperRemote{
			name:     "git-remote-object-store",
			describe: "dekobon/git-remote-object-store helper (packchain engine): a manifest of packs",
			binary:   "git-remote-s3+http",
			template: "s3+http://{host}/{bucket}/{prefix}/git-remote-object-store/{repo}?addressing=path&region={region}&engine=packchain",
			extraEnv: []string{"GIT_REMOTE_OBJECT_STORE_ALLOW_HTTP=1"},
		}
	})
}

// localRemote is a bare repository on local disk reached by path: stock git on
// both ends and no network at all. It is the ceiling of this level, as go-git in
// memory is of the other.
type localRemote struct {
	root string
}

func (d *localRemote) Name() string { return "git-local" }
func (d *localRemote) Describe() string {
	return "stock git, bare repository on local disk: the ceiling"
}
func (d *localRemote) Available() error { return nil }

func (d *localRemote) Setup(_ context.Context, env Env) error {
	root, err := env.tempDir("git-local-*")
	d.root = root
	return err
}

func (d *localRemote) Remote(ctx context.Context, repo string) (string, error) {
	dir := filepath.Join(d.root, filepath.FromSlash(repo)+".git")
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", err
	}
	_, err := runGit(ctx, "", nil, nil, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	return dir, err
}

func (d *localRemote) GitEnv() []string              { return nil }
func (d *localRemote) Restart(context.Context) error { return nil }
func (d *localRemote) Close() error                  { return nil }

// helperRemote is a git remote helper: a program named git-remote-<scheme> that
// git runs for URLs of that scheme. It is configured by a URL template and the
// standard AWS environment, which is how every helper surveyed takes its
// endpoint, so a helper not listed here can be driven from the command line
// with -remote name=template and no code.
//
// Placeholders: {endpoint} the metered endpoint URL, {host} its host:port,
// {bucket}, {prefix} the run's key prefix, {region}, {repo}.
type helperRemote struct {
	name     string
	describe string
	binary   string
	template string
	extraEnv []string
	// pathConf writes an AWS config file selecting path-style addressing, for
	// helpers built on an AWS SDK that would otherwise put the bucket in the
	// host name, which no local endpoint answers to.
	pathConf bool

	env     Env
	confDir string
}

func (d *helperRemote) Name() string     { return d.name }
func (d *helperRemote) Describe() string { return d.describe }

func (d *helperRemote) Available() error {
	if d.binary == "" {
		return nil
	}
	if _, err := exec.LookPath(d.binary); err != nil {
		return fmt.Errorf("%s is not on PATH", d.binary)
	}
	return nil
}

func (d *helperRemote) Setup(_ context.Context, env Env) error {
	d.env = env
	if !d.pathConf {
		return nil
	}
	dir, err := env.tempDir("aws-config-*")
	if err != nil {
		return err
	}
	d.confDir = dir
	return os.WriteFile(filepath.Join(dir, "config"), []byte("[default]\nregion = "+env.Region+"\ns3 =\n  addressing_style = path\n"), 0o600)
}

func (d *helperRemote) Remote(_ context.Context, repo string) (string, error) {
	endpoint, err := url.Parse(d.env.Endpoint)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer(
		"{endpoint}", d.env.Endpoint, "{host}", endpoint.Host, "{bucket}", d.env.Bucket,
		"{prefix}", d.env.Prefix, "{region}", d.env.Region, "{repo}", repo,
	).Replace(d.template), nil
}

func (d *helperRemote) GitEnv() []string {
	env := []string{
		"AWS_ACCESS_KEY_ID=" + d.env.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + d.env.SecretKey,
		"AWS_DEFAULT_REGION=" + d.env.Region,
		"AWS_REGION=" + d.env.Region,
		"AWS_ENDPOINT_URL=" + d.env.Endpoint,
		"AWS_ENDPOINT_URL_S3=" + d.env.Endpoint,
		// Instance-metadata lookups would stall a run on a laptop and mean
		// nothing on a build host.
		"AWS_EC2_METADATA_DISABLED=true",
	}
	if d.confDir != "" {
		env = append(env, "AWS_CONFIG_FILE="+filepath.Join(d.confDir, "config"))
	}
	return append(env, d.extraEnv...)
}

func (d *helperRemote) Restart(context.Context) error { return nil }
func (d *helperRemote) Close() error                  { return nil }

// runGit runs the stock git client, isolated from the operator's configuration
// so that a credential helper, a URL rewrite or a protocol override in their
// ~/.gitconfig cannot change what is measured.
func runGit(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	// #nosec G204 -- git is a fixed executable and exec.Command passes each
	// argument verbatim, with no shell; the arguments are the harness's own
	// subcommands, scratch paths and the remote URLs it built.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true",
	)
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w\n%s", redactCredentials(strings.Join(args, " ")), err,
			redactCredentials(strings.TrimSpace(string(out))))
	}
	return string(out), nil
}

var urlCredentials = regexp.MustCompile(`://[^/@\s]+@`)

// redactCredentials strips user:password@ from URLs. A remote's URL can carry a
// token, and an error message is copied into reports, logs and bug trackers.
func redactCredentials(text string) string {
	return urlCredentials.ReplaceAllString(text, "://")
}
