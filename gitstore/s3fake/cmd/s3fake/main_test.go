package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheTraceNamesARequestWithoutItsSecrets pins both halves of what a trace
// line is for. It must tell one request from another — which listing, which
// extent, whether the write was conditional — and it must be safe to paste into
// a bug report: a presigned URL carries its credential and signature in the
// query, and a client's headers are its own business.
func TestTheTraceNamesARequestWithoutItsSecrets(t *testing.T) {
	list := httptest.NewRequest(http.MethodGet,
		"/bucket/?list-type=2&prefix=repo%2Frefs%2F&delimiter=%2F&X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeef", nil)
	if got, want := traceLine(list), "GET /bucket/ prefix=repo/refs/ delimiter=/"; got != want {
		t.Fatalf("listing traced as %q, want %q", got, want)
	}

	read := httptest.NewRequest(http.MethodGet, "/bucket/objects/pack/pack-1.pack", nil)
	read.Header.Set("Range", "bytes=0-4194303")
	read.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE")
	if got, want := traceLine(read), "GET /bucket/objects/pack/pack-1.pack [bytes 0-4194303]"; got != want {
		t.Fatalf("ranged read traced as %q, want %q", got, want)
	}

	for header, want := range map[string]string{"If-Match": "[if unchanged]", "If-None-Match": "[if changed]"} {
		swap := httptest.NewRequest(http.MethodPut, "/bucket/manifest", nil)
		swap.Header.Set(header, `"0123456789abcdef"`)
		got := traceLine(swap)
		if !strings.HasSuffix(got, want) || strings.Contains(got, "0123456789abcdef") {
			t.Fatalf("%s traced as %q, want it to end %q and keep the tag out", header, got, want)
		}
	}
	lock := httptest.NewRequest(http.MethodPut, "/bucket/lock", nil)
	lock.Header.Set("If-None-Match", "*")
	if got, want := traceLine(lock), "PUT /bucket/lock [if absent]"; got != want {
		t.Fatalf("lock traced as %q, want %q", got, want)
	}

	forged := httptest.NewRequest(http.MethodGet, "/bucket/?prefix=a%0A2026%2F09%2F20+DELETE+%2Feverything", nil)
	if got := traceLine(forged); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("a client started a line of its own: %q", got)
	}
}
