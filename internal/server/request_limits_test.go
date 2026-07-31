package bleephub

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStructuredRequestBodiesAreBoundedAtProductionPipeline(t *testing.T) {
	server := newTestServer()
	server.mux.HandleFunc("POST /bounded", func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/bounded", bytes.NewReader(make([]byte, maxStructuredRequestBody+1)))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	recorder := httptest.NewRecorder()
	server.requestHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestStreamingRequestBodiesKeepRouteSpecificLimits(t *testing.T) {
	server := newTestServer()
	server.mux.HandleFunc("POST /stream", func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Fatalf("read streaming body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewReader(make([]byte, maxStructuredRequestBody+1)))
	request.Header.Set("Content-Type", "application/octet-stream")
	recorder := httptest.NewRecorder()
	server.requestHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}
