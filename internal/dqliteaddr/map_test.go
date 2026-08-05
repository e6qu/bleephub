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

// TestListenAddr covers STORE-055: the listen address must default to the
// advertised port rather than a fixed ":9000", or a node advertises one port to
// peers while listening on another and peers cannot connect.
func TestListenAddr(t *testing.T) {
	// Explicit override wins (NAT/proxy).
	if got, err := ListenAddr("0.0.0.0:1234", "host.internal:7000"); err != nil || got != "0.0.0.0:1234" {
		t.Fatalf("override: got %q err %v, want 0.0.0.0:1234", got, err)
	}
	// Whitespace override is treated as unset.
	if got, err := ListenAddr("   ", "host.internal:8100"); err != nil || got != ":8100" {
		t.Fatalf("blank override: got %q err %v, want :8100", got, err)
	}
	// Default derives the listen port from the advertised port, not a fixed :9000.
	if got, err := ListenAddr("", "host.internal:7000"); err != nil || got != ":7000" {
		t.Fatalf("STORE-055: default listen = %q err %v, want :7000 (must track the advertised port)", got, err)
	}
	// A malformed advertise address is an error, not a silent fallback.
	if _, err := ListenAddr("", "no-port-here"); err == nil {
		t.Fatal("expected an error for an advertise address with no port")
	}
}
