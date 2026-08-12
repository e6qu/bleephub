package actions

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJobOutputsTokenUsesRunnerMappingShape(t *testing.T) {
	got := jobOutputsToken(map[string]string{
		"version": "${{ steps.ver.outputs.version }}",
		"channel": "stable",
	})
	want := mappingToken([]interface{}{
		mappingEntry("channel", templateToken("stable")),
		mappingEntry("version", templateToken("${{ steps.ver.outputs.version }}")),
	})
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal job outputs token: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected job outputs token: %v", err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("jobOutputs = %s, want %s", gotJSON, wantJSON)
	}
	if got := jobOutputsToken(nil); got != nil {
		t.Fatalf("empty jobOutputs = %#v, want nil", got)
	}
}
