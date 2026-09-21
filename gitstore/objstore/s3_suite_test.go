package objstore_test

import (
	"testing"

	"github.com/e6qu/bleephub/gitstore/objstore"
	"github.com/e6qu/bleephub/gitstore/objstore/objstoretest"
)

// TestTheS3DriverPassesTheSuite holds the S3 driver to what every driver must
// do, against the in-process S3.
func TestTheS3DriverPassesTheSuite(t *testing.T) {
	objstoretest.Run(t, func(t *testing.T) objstore.Bucket {
		bucket, _ := newBucket(t)
		return bucket
	})
}
