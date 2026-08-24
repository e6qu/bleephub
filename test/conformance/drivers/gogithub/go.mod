// Conformance driver module for the official google/go-github client.
//
// It is a separate module (like sdk-tests/) on purpose: the driver is a real
// third-party consumer of Bleephub, so it must resolve the SDK from the module
// proxy exactly as a user would rather than inherit the server module's
// dependency graph. The go-github version is pinned to the same release
// sdk-tests already pins, which scripts/check-dependency-age.py vets.
module github.com/e6qu/bleephub-conformance-gogithub

go 1.25.12

require github.com/google/go-github/v88 v88.0.0

require github.com/google/go-querystring v1.2.0 // indirect
