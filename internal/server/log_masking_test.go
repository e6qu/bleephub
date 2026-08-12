package bleephub

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogMasking_SecretVariablesAndAddMaskCommandsAreScrubbed(t *testing.T) {
	store := NewStore()
	message := map[string]interface{}{
		"variables": map[string]interface{}{
			"PUBLIC": map[string]interface{}{"value": "visible", "isSecret": false},
			"TOKEN":  map[string]interface{}{"value": "known-secret", "isSecret": true},
		},
	}
	store.Mu.Lock()
	store.RegisterJobLogMasksLocked("plan-1", message)
	got := store.RedactLogBytesLocked("plan-1", []byte(strings.Join([]string{
		"known-secret must be hidden",
		"::add-mask::dynamic%25secret",
		"dynamic%secret must also be hidden",
		"visible remains visible",
	}, "\n")))
	store.Mu.Unlock()

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
	store := NewStore()
	store.Mu.Lock()
	first := store.RedactLogLinesLocked("plan-1", []string{"##[add-mask]console-secret"})
	second := store.RedactLogLinesLocked("plan-1", []string{"echo console-secret"})
	store.Mu.Unlock()

	if got := first[0]; got != "##[add-mask]***" {
		t.Fatalf("mask command = %q", got)
	}
	if got := second[0]; got != "echo ***" {
		t.Fatalf("later console line = %q", got)
	}
}
