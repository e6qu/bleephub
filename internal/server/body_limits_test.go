package bleephub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An upload/body read must refuse a body larger than its limit rather than
// buffering it unbounded (STORE-019).
func TestReadLimitedBodyRejectsOversizedBody(t *testing.T) {
	oversized := httptest.NewRequest("POST", "/x", strings.NewReader(strings.Repeat("x", 100)))
	rec := httptest.NewRecorder()
	if _, ok := readLimitedBody(rec, oversized, 10); ok {
		t.Fatal("readLimitedBody accepted a body over its limit")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", rec.Code)
	}

	within := httptest.NewRequest("POST", "/x", strings.NewReader("small"))
	data, ok := readLimitedBody(httptest.NewRecorder(), within, 100)
	if !ok || string(data) != "small" {
		t.Fatalf("within-limit read failed: ok=%v data=%q", ok, data)
	}
}

// decodeJSONBody caps the JSON body and reports 413 rather than decoding an
// unbounded payload.
func TestDecodeJSONBodyRejectsOversizedBody(t *testing.T) {
	huge := strings.Repeat("a", maxJSONBodyBytes+1)
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"`+huge+`"}`))
	rec := httptest.NewRecorder()
	var v map[string]any
	if decodeJSONBody(rec, req, &v) {
		t.Fatal("decodeJSONBody accepted a body over the cap")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON status = %d, want 413", rec.Code)
	}

	ok := httptest.NewRecorder()
	small := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"ok"}`))
	var v2 map[string]any
	if !decodeJSONBody(ok, small, &v2) || v2["name"] != "ok" {
		t.Fatalf("within-limit JSON decode failed: %v", v2)
	}
}
