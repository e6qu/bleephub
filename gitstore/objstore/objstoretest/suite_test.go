package objstoretest_test

import (
	"testing"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/objstore/objstoretest"
	"github.com/e6qu/bleephub/gitstore/s3fake"
)

// TestTheSuiteHoldsADriverItIsKnownToFit runs the suite from its own package,
// against the S3 driver over the in-process S3. Each driver runs it again beside
// its own code; this run is what makes a change to the suite answer for itself,
// here, before it is some driver's failure somewhere else.
func TestTheSuiteHoldsADriverItIsKnownToFit(t *testing.T) {
	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		server := s3fake.New()
		t.Cleanup(server.Close)
		return objstore.NewS3WithClient(server.Client().Client, "bucket", 0)
	})
}
