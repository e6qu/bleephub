package bleephub

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeLogTextPreventsForgedEntries(t *testing.T) {
	t.Parallel()

	const malicious = "owner/repo\r\nbleephub: forged entry"
	got := safeLogText(malicious)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("safeLogText retained a line ending: %q", got)
	}
	if want := `owner/repo\r\nbleephub: forged entry`; got != want {
		t.Fatalf("safeLogText() = %q, want %q", got, want)
	}
	if gotErr := safeLogError(errors.New(malicious)); gotErr != got {
		t.Fatalf("safeLogError() = %q, want %q", gotErr, got)
	}
	if gotNil := safeLogError(nil); gotNil != "" {
		t.Fatalf("safeLogError(nil) = %q, want empty string", gotNil)
	}
}
