package bleephub

import (
	"bytes"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestLogMasking_SecretVariablesAndAddMaskCommandsAreScrubbed(t *testing.T) {
	st := store.NewStore()
	message := map[string]interface{}{
		"variables": map[string]interface{}{
			"PUBLIC": map[string]interface{}{"value": "visible", "isSecret": false},
			"TOKEN":  map[string]interface{}{"value": "known-secret", "isSecret": true},
		},
	}
	st.Mu.Lock()
	st.RegisterJobLogMasksLocked("plan-1", message)
	got := st.RedactLogBytesLocked("plan-1", []byte(strings.Join([]string{
		"known-secret must be hidden",
		"::add-mask::dynamic%25secret",
		"dynamic%secret must also be hidden",
		"visible remains visible",
	}, "\n")))
	st.Mu.Unlock()

	if bytes.Contains(got, []byte("known-secret")) || bytes.Contains(got, []byte("dynamic%secret")) {
		t.Fatalf("redacted log disclosed a mask: %q", got)
	}
	for _, want := range []string{"*** must be hidden", "::add-mask::***", "*** must also be hidden", "visible remains visible"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("redacted log %q does not contain %q", got, want)
		}
	}
}

func TestLogMasking_ConsoleMaskPersistsForLaterLines(t *testing.T) {
	st := store.NewStore()
	st.Mu.Lock()
	first := st.RedactLogLinesLocked("plan-1", []string{"##[add-mask]console-secret"})
	second := st.RedactLogLinesLocked("plan-1", []string{"echo console-secret"})
	st.Mu.Unlock()

	if got := first[0]; got != "##[add-mask]***" {
		t.Fatalf("mask command = %q", got)
	}
	if got := second[0]; got != "echo ***" {
		t.Fatalf("later console line = %q", got)
	}
}
