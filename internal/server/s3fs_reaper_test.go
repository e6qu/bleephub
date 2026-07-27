package bleephub

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// createLabeledS3Container creates (without starting) a container carrying the
// owner label, and returns its id. Creating rather than running is enough: the
// reaper selects on the label and the recorded owner, and a created container
// is listed by `docker ps --all` exactly as an abandoned running one is.
func createLabeledS3Container(t *testing.T, owner int) string {
	t.Helper()
	output, err := exec.Command("docker", "create",
		"--label", s3TestOwnerLabel+"="+strconv.Itoa(owner),
		"minio/minio:RELEASE.2025-04-22T22-12-26Z", "server", "/data").CombinedOutput()
	if err != nil {
		t.Fatalf("create labeled container: %v\n%s", err, output)
	}
	id := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })
	return id
}

func containerExists(id string) bool {
	return exec.Command("docker", "inspect", id).Run() == nil
}

// TestReapAbandonedS3Servers pins both halves of the reaper. Removing an
// abandoned container is the point; leaving a live one alone is what makes the
// reaper safe to run unconditionally at suite start, because several suites
// share a developer's machine and a reaper that removed every labeled container
// would pull the S3 server out from under a concurrently running suite.
func TestReapAbandonedS3Servers(t *testing.T) {
	// A process id that is certainly gone: run a trivial command to completion
	// and take its id.
	finished := exec.Command("/usr/bin/true")
	if err := finished.Run(); err != nil {
		t.Fatalf("run /usr/bin/true: %v", err)
	}
	abandonedOwner := finished.Process.Pid
	if testBinaryAlive(abandonedOwner) {
		t.Fatalf("process id %d was reused immediately after exiting; rerun", abandonedOwner)
	}

	abandoned := createLabeledS3Container(t, abandonedOwner)
	live := createLabeledS3Container(t, os.Getpid())

	reapAbandonedS3Servers()

	if containerExists(abandoned) {
		t.Fatalf("a container whose owning test binary has exited survived the reaper (%s)", abandoned)
	}
	if !containerExists(live) {
		t.Fatalf("a container owned by a running test binary was reaped (%s) — "+
			"a concurrently running suite would lose its S3 server", live)
	}
}

// TestS3ServerCarriesItsOwner pins the label the reaper selects on: without it
// an abandoned container is indistinguishable from any other MinIO on the host.
//
// It asserts the argument vector rather than inspecting the running server,
// because the shared container's lifetime is not this package's to guarantee —
// a test that asserts on it reports someone else's teardown as a failure of
// the label contract.
func TestS3ServerCarriesItsOwner(t *testing.T) {
	args := s3ServerRunArgs("127.0.0.1:9999")
	want := s3TestOwnerLabel + "=" + strconv.Itoa(os.Getpid())
	for i, arg := range args {
		if arg == "--label" {
			if i+1 >= len(args) {
				t.Fatalf("--label is the last argument, with no value: %v", args)
			}
			if got := args[i+1]; got != want {
				t.Fatalf("owner label = %q, want the test binary's own id %q", got, want)
			}
			return
		}
	}
	t.Fatalf("the MinIO server is started without an owner label, so the reaper "+
		"cannot tell an abandoned container from any other MinIO on the host: %v", args)
}
