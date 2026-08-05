package dqliteaddr

import "testing"

func TestFromEnvironment(t *testing.T) {
	mapping, err := FromEnvironment(" legacy.example:9000 = dqlite-0.internal:9000,legacy.example:9001=dqlite-1.internal:9000 ")
	if err != nil {
		t.Fatalf("parse address map: %v", err)
	}
	if got, want := mapping.Resolve("legacy.example:9000"), "dqlite-0.internal:9000"; got != want {
		t.Fatalf("resolved address = %q, want %q", got, want)
	}
	if got, want := mapping.Resolve("other.example:9000"), "other.example:9000"; got != want {
		t.Fatalf("unmapped address = %q, want %q", got, want)
	}
}

func TestFromEnvironmentRejectsInvalidEntries(t *testing.T) {
	for _, value := range []string{"legacy.example:9000", "=dqlite-0.internal:9000", "legacy.example:9000=", "legacy.example:9000=a,legacy.example:9000=b"} {
		if _, err := FromEnvironment(value); err == nil {
			t.Fatalf("FromEnvironment(%q) succeeded", value)
		}
	}
}
