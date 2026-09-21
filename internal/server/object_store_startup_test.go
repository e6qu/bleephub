package bleephub

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/server/testutil"
	"github.com/rs/zerolog"
)

// TestStartupRefusesAGitObjectStoreThatFailsConformance pins that the probe's
// verdict is fatal where an operator will see it. A server that logged the
// failure and served anyway would run correctly until two replicas raced for one
// branch, and then lose a push without either of them knowing.
func TestStartupRefusesAGitObjectStoreThatFailsConformance(t *testing.T) {
	t.Setenv("BLEEPHUB_SSH_ADDR", "")
	t.Setenv("BLEEPHUB_GIT_DIR", "")
	testutil.ClearObjectStoreSettings(t)
	testutil.ConfigureS3ForTest(t)
	t.Setenv("BLEEPHUB_OBJECT_STORE", "s3")
	t.Setenv("BLEEPHUB_S3_ENDPOINT", testutil.S3EndpointIgnoringConditions(t))
	t.Setenv("BLEEPHUB_GIT_BUCKET", "bleephub-test")
	t.Setenv("BLEEPHUB_GIT_PREFIX", "git")
	t.Setenv("BLEEPHUB_GITSTORE_CACHE_DIR", t.TempDir())
	resetGitObjectStoreForTest(t)

	srv := NewServer("127.0.0.1:0", zerolog.New(io.Discard))
	err := srv.ListenAndServe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not conform") {
		t.Fatalf("startup on a store that ignores conditions answered %v, want a conformance refusal", err)
	}
}
