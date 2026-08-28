package bleephub

import (
	"io"
	"net/http"
	"testing"
)

// The profile/org Packages tab lists every package type at once. GitHub's public
// REST list endpoints require a package_type (400 otherwise), so the tab reads
// the /ui-data aggregations, which must return 200 and every type.
func TestUIPackagesListAllTypesWithoutPackageType(t *testing.T) {
	t.Parallel()
	s := newIsolatedServer(t)
	admin := s.store.UsersByLogin["admin"]
	s.seedPackageVersion(t, "user", admin.Login, "npm", "u-pkg", "1.0.0")
	org := s.store.CreateOrg(admin, "pkg-org", "Pkg Org", "")
	s.seedPackageVersion(t, "org", org.Login, "npm", "org-pkg", "2.0.0")

	for _, tc := range []struct{ path, want string }{
		{"/ui-data/users/" + admin.Login + "/packages", "u-pkg"},
		{"/ui-data/orgs/" + org.Login + "/packages", "org-pkg"},
	} {
		resp := s.get(t, tc.path, defaultToken)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s = %d, want 200: %s", tc.path, resp.StatusCode, b)
		}
		list := decodeJSONArray(t, resp)
		found := false
		for _, p := range list {
			if p["name"] == tc.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s did not list %q: %v", tc.path, tc.want, list)
		}
	}
}
