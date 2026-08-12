package graphqlapi

import (
	"os"
	"testing"
)

// defaultTestAdminToken mirrors the server test harness's defaultToken: the
// store's default-admin seeding requires BLEEPHUB_ADMIN_TOKEN explicitly (the
// admin token has no default).
const defaultTestAdminToken = "bleephub-admin-token-00000000000000000000"

func TestMain(m *testing.M) {
	os.Setenv("BLEEPHUB_ADMIN_TOKEN", defaultTestAdminToken)
	os.Exit(m.Run())
}
