package store

import (
	"reflect"
	"testing"
)

func TestDockerStopArgsUseCurrentTimeoutFlag(t *testing.T) {
	t.Parallel()
	want := []string{"stop", "--timeout", "30", "container-id"}
	if got := dockerStopArgs("container-id"); !reflect.DeepEqual(got, want) {
		t.Fatalf("docker stop arguments = %q, want %q", got, want)
	}
}
