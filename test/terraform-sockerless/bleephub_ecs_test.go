package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

var simulatorURL string
var simulator *exec.Cmd
var sockerlessRepository string

func TestMain(m *testing.M) {
	var err error
	sockerlessRepository, err = resolveSockerlessRepository()
	if err != nil {
		panic(fmt.Sprintf("locate sockerless repository: %v", err))
	}
	simDir := filepath.Join(sockerlessRepository, "simulators", "aws")
	binary := filepath.Join(os.TempDir(), "simulator-aws-bleephub-ecs-test")
	build := exec.Command("go", "build", "-tags", "noui", "-o", binary, ".")
	build.Dir = simDir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOCACHE=/private/tmp/sockerless-go-cache")
	if out, err := build.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build AWS simulator: %v\n%s", err, out))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	simulator = exec.Command(binary)
	simulator.Env = append(os.Environ(), fmt.Sprintf("SIM_LISTEN_ADDR=:%d", port))
	simulator.Stdout, simulator.Stderr = io.Discard, io.Discard
	if err := simulator.Start(); err != nil {
		panic(err)
	}
	simulatorURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.NewTimer(10 * time.Second)
	retry := time.NewTicker(100 * time.Millisecond)
ready:
	for {
		response, err := http.Get(simulatorURL + "/health") // #nosec G107 -- test-local simulator coordinate
		if err == nil && response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			break ready
		}
		if response != nil {
			_ = response.Body.Close()
		}
		select {
		case <-deadline.C:
			break ready
		case <-retry.C:
		}
	}
	deadline.Stop()
	retry.Stop()
	code := m.Run()
	_ = simulator.Process.Kill()
	_, _ = simulator.Process.Wait()
	_ = os.Remove(binary)
	cleanupSockerlessRepository()
	os.Exit(code)
}

// sockerlessPinnedSHA pins the sockerless simulator source to an exact commit
// so this test is reproducible and cannot be silently retargeted by an upstream
// push (TEST-024). The commit hash is itself the integrity check. Bump it
// deliberately when the pinned behaviour needs to change.
const sockerlessPinnedSHA = "8cb5c17d1b510b0d6be3853365ea0fcc77077d89"

func resolveSockerlessRepository() (string, error) {
	if configured := os.Getenv("SOCKERLESS_REPOSITORY"); configured != "" {
		return configured, nil
	}
	parent, err := os.MkdirTemp("", "bleephub-sockerless-")
	if err != nil {
		return "", err
	}
	checkout := filepath.Join(parent, "sockerless")
	// Fetch exactly the pinned commit (GitHub honours fetch-by-sha) rather than
	// whatever a moving branch tip happens to be at test time.
	for _, args := range [][]string{
		{"init", "--quiet", checkout},
		{"-C", checkout, "remote", "add", "origin", "https://github.com/e6qu/sockerless.git"},
		{"-C", checkout, "fetch", "--quiet", "--depth=1", "origin", sockerlessPinnedSHA},
		{"-C", checkout, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			_ = os.RemoveAll(parent)
			return "", fmt.Errorf("fetch pinned sockerless simulator source (git %v): %w\n%s", args, err, output)
		}
	}
	return checkout, nil
}

func cleanupSockerlessRepository() {
	if os.Getenv("SOCKERLESS_REPOSITORY") == "" && sockerlessRepository != "" {
		_ = os.RemoveAll(filepath.Dir(sockerlessRepository))
	}
}

func TestBleephubECSApplyDestroy(t *testing.T) {
	moduleDir, err := filepath.Abs("../../terraform")
	if err != nil {
		t.Fatal(err)
	}
	wakeZip := filepath.Join(t.TempDir(), "wake.zip")
	buildWake(t, wakeZip)
	startupPage := filepath.Join(t.TempDir(), "startup.html")
	buildStartupPage(t, startupPage)
	dir := t.TempDir()
	root := fmt.Sprintf(`
terraform {
  required_providers { aws = { source = "hashicorp/aws", version = "~> 6.0" } }
}
provider "aws" {
  region = "eu-west-1"
  access_key = "test"
  secret_key = "test"
  skip_credentials_validation = true
  skip_metadata_api_check = true
  skip_requesting_account_id = true
  endpoints {
    acm = "%[1]s"
    apigatewayv2 = "%[1]s"
    autoscaling = "%[1]s"
    cloudfront = "%[1]s"
    cloudwatch = "%[1]s"
    cloudwatchlogs = "%[1]s"
    ec2 = "%[1]s"
    ecs = "%[1]s"
    efs = "%[1]s"
    elasticloadbalancing = "%[1]s"
    iam = "%[1]s"
    kms = "%[1]s"
    lambda = "%[1]s"
    route53 = "%[1]s"
    scheduler = "%[1]s"
    servicediscovery = "%[1]s"
    s3 = "%[1]s"
    secretsmanager = "%[1]s"
    ssm = "%[1]s"
    sts = "%[1]s"
  }
}
resource "aws_route53_zone" "bleephub" {
  name = "bleephub.example.test"
}
module "bleephub" {
  source = %q
  name = "bleephub-test"
  region = "eu-west-1"
  availability_zones = ["eu-west-1a", "eu-west-1b"]
  hosted_zone_id = aws_route53_zone.bleephub.zone_id
  domain_name = "bleephub.example.test"
  container_image = "public.ecr.aws/docker/library/alpine:3.20"
  ssh_ingress_cidr_blocks = ["203.0.113.0/24"]
  admin_token = "test-administrator-token"
  idle_shutdown_enabled = false
  dqlite_advertise_addresses = {
    "0" = "legacy-dqlite.example.test:9000"
    "1" = "legacy-dqlite.example.test:9001"
    "2" = "legacy-dqlite.example.test:9002"
  }
  wake_listener_zip_path = %q
  startup_page_path = %q
}
`, simulatorURL, moduleDir, wakeZip, startupPage)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	runTerraform(t, dir, "init", "-backend=false")
	runTerraform(t, dir, "apply", "-auto-approve")
	idleShutdown := runTerraformOutput(t, dir, "state", "show", "module.bleephub.aws_lambda_function.idle_shutdown")
	if !strings.Contains(idleShutdown, "timeout                         = 300") {
		t.Fatalf("idle shutdown Lambda did not retain the 300-second control-plane timeout:\n%s", idleShutdown)
	}
	wake := runTerraformOutput(t, dir, "state", "show", "module.bleephub.aws_lambda_function.wake")
	if !strings.Contains(wake, "ADMIN_TOKEN_SECRET_ARN") {
		t.Fatalf("wake Lambda did not receive the administrator startup-dashboard credential coordinate:\n%s", wake)
	}
	startup := runTerraformOutput(t, dir, "state", "show", "module.bleephub.aws_s3_object.startup_page")
	if !strings.Contains(startup, "startup/index.html") || !strings.Contains(startup, "text/html; charset=utf-8") {
		t.Fatalf("S3 startup document was not uploaded with its explicit browser content type:\n%s", startup)
	}
	service := runTerraformOutput(t, dir, "state", "show", "module.bleephub.aws_ecs_service.this")
	if !strings.Contains(service, "desired_count") || !strings.Contains(service, "= 1") {
		t.Fatalf("always-on Bleephub application service did not start with one task:\n%s", service)
	}
	dqlite := runTerraformOutput(t, dir, "state", "show", `module.bleephub.aws_ecs_service.dqlite["0"]`)
	if !strings.Contains(dqlite, "desired_count") || !strings.Contains(dqlite, "= 1") {
		t.Fatalf("always-on Bleephub dqlite service did not start with one task:\n%s", dqlite)
	}
	if strings.Contains(dqlite, "container_port") {
		t.Fatalf("Bleephub dqlite A-record discovery service must not set an Amazon ECS service-registry port:\n%s", dqlite)
	}
	dqliteTask := runTerraformOutput(t, dir, "state", "show", `module.bleephub.aws_ecs_task_definition.dqlite["0"]`)
	if !strings.Contains(dqliteTask, "legacy-dqlite.example.test:9000") || !strings.Contains(dqliteTask, "BLEEPHUB_DQLITE_ADDRESS_MAP") || !strings.Contains(dqliteTask, "dqlite-0.bleephub-test.internal:9000") {
		t.Fatalf("DQLite durable-address migration did not preserve the legacy identity and translate it to Cloud Map:\n%s", dqliteTask)
	}
	discovery := runTerraformOutput(t, dir, "state", "show", "module.bleephub.aws_service_discovery_service.app")
	if !strings.Contains(discovery, `name                            = "app"`) || !strings.Contains(discovery, `type = "SRV"`) {
		t.Fatalf("Bleephub application did not register its direct Amazon ECS discovery endpoint:\n%s", discovery)
	}
	// The wake controller is the only writer of desired_count once the service
	// exists. Reconciling it here would stop whatever the controller started, so
	// a plan must stay clean across an out-of-band capacity change.
	setServiceDesiredCount(t, "bleephub-test", 0)
	if output, exitCode := runTerraformWithExitCode(t, dir, "plan", "-detailed-exitcode"); exitCode != 0 {
		t.Fatalf("Terraform tried to reclaim application capacity the wake controller owns (exit %d):\n%s", exitCode, output)
	}

	// Every store that holds the only copy of user data carries prevent_destroy,
	// which Terraform accepts only as a literal. A teardown therefore releases
	// them from state first, and this proves the guard is what stops it.
	guarded := []string{
		"module.bleephub.aws_kms_key.this",
		"module.bleephub.aws_s3_bucket.git",
		"module.bleephub.aws_s3_bucket.objects",
		"module.bleephub.aws_efs_file_system.sqlite",
		"module.bleephub.aws_secretsmanager_secret.admin_token",
		"module.bleephub.aws_secretsmanager_secret.ssh_host_key",
		"module.bleephub.aws_secretsmanager_secret.dqlite_secret",
	}
	output, exitCode := runTerraformWithExitCode(t, dir, "plan", "-destroy")
	if exitCode == 0 || !strings.Contains(output, "prevent_destroy") {
		t.Fatalf("a destroy of the durable stores was not refused (exit %d):\n%s", exitCode, output)
	}
	for _, address := range guarded {
		if !strings.Contains(output, address) {
			t.Fatalf("%s is not protected against destroy:\n%s", address, output)
		}
	}
	runTerraform(t, dir, append([]string{"state", "rm"}, guarded...)...)
	runTerraform(t, dir, "destroy", "-auto-approve")
}

func setServiceDesiredCount(t *testing.T, service string, desiredCount int32) {
	t.Helper()
	client := ecs.New(ecs.Options{
		Region:       "eu-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(simulatorURL),
	})
	_, err := client.UpdateService(context.Background(), &ecs.UpdateServiceInput{
		Cluster:      aws.String("bleephub-test"),
		Service:      aws.String(service),
		DesiredCount: aws.Int32(desiredCount),
	})
	if err != nil {
		t.Fatalf("set ECS service desired count: %v", err)
	}
}

func buildStartupPage(t *testing.T, destination string) {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repo, "scripts", "build-bleephub-startup.sh"), filepath.Dir(destination))
	command.Env = append(os.Environ(), "BLEEPHUB_VERSION=test-build", "BLEEPHUB_PUBLISHED_AT=2026-07-14T00:00:00Z")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build startup page: %v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(filepath.Dir(destination), "startup", "index.html"), destination); err != nil {
		t.Fatal(err)
	}
}

func buildWake(t *testing.T, destination string) {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(repo, "scripts", "build-bleephub-wake.sh"), filepath.Dir(destination))
	command.Env = append(os.Environ(), "GOCACHE=/private/tmp/sockerless-go-cache")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build wake listener: %v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(filepath.Dir(destination), "bleephub-wake.zip"), destination); err != nil {
		t.Fatal(err)
	}
}

func runTerraform(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	_ = runTerraformOutput(t, directory, arguments...)
}

func runTerraformOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("terraform", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "AWS_ENDPOINT_URL="+simulatorURL, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_DEFAULT_REGION=eu-west-1", "TF_LOG=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("terraform %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func runTerraformWithExitCode(t *testing.T, directory string, arguments ...string) (string, int) {
	t.Helper()
	command := exec.Command("terraform", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "AWS_ENDPOINT_URL="+simulatorURL, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_DEFAULT_REGION=eu-west-1", "TF_LOG=")
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("terraform %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output), exitError.ExitCode()
}
