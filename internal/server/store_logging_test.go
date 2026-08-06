package bleephub

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	gitStorage "github.com/go-git/go-git/v5/storage"
	"github.com/rs/zerolog"
)

// TestStoreErrorsLogThroughStructuredLogger covers CORE-025: store-layer error
// logging goes through the wired structured logger (level filter + telemetry
// bridge) as discrete fields, not stdlib log.Printf with an interpolated
// plain-text message.
func TestStoreErrorsLogThroughStructuredLogger(t *testing.T) {
	st := NewStore()
	admin := &User{ID: 1, Login: "admin", Type: "User"}
	st.Users[admin.ID] = admin
	st.UsersByLogin[admin.Login] = admin

	var buf bytes.Buffer
	st.logger = zerolog.New(&buf)
	st.repoStorageOpen = func(context.Context, string) (gitStorage.Storer, error) {
		return nil, errors.New("disk on fire")
	}

	if got := st.CreateRepo(admin, "doomed", "", false); got != nil {
		t.Fatalf("CreateRepo returned %v despite a storage error", got)
	}

	line := buf.String()
	for _, want := range []string{
		`"level":"error"`,
		`"repo":"admin/doomed"`,
		`"error":"disk on fire"`,
		"create repo: open git storage failed",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("structured log line %q missing %q", line, want)
		}
	}
}

// TestNewStoreLoggerIsNopByDefault: a bare store (as tests construct) must not
// write to stderr — the logger defaults to nop until NewServer wires the
// configured one.
func TestNewStoreLoggerIsNopByDefault(t *testing.T) {
	st := NewStore()
	if st.logger.GetLevel() != zerolog.Disabled {
		t.Fatalf("NewStore logger level = %v, want Disabled (nop)", st.logger.GetLevel())
	}
}
