package bleephub

import "testing"

func TestDependabotVersionRangesHandlePackageVersionDialects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		rule    string
		want    bool
	}{
		{"1.2.3", ">= 1.0.0, < 2.0.0", true},
		{"2.0.0", ">= 1.0.0, < 2.0.0", false},
		{"1.0.0-rc.1", "< 1.0.0", true},
		{"2.4.0rc1", "< 2.4.0", true},
		{"1.0.0+build.9", "= 1.0.0", true},
		{"release-candidate", "< 2.0.0", true}, // unknown dialect fails safe
	}
	for _, test := range tests {
		test := test
		t.Run(test.version+"/"+test.rule, func(t *testing.T) {
			t.Parallel()
			if got := dependabotVersionInRange(test.version, test.rule); got != test.want {
				t.Fatalf("dependabotVersionInRange(%q, %q) = %v, want %v", test.version, test.rule, got, test.want)
			}
		})
	}
}
