package gitstore

import "testing"

func TestBleephubS3Region(t *testing.T) {
	t.Setenv("BLEEPHUB_S3_REGION", "eu-west-1")
	t.Setenv("AWS_REGION", "us-east-1")
	if got := bleephubS3Region(); got != "eu-west-1" {
		t.Fatalf("explicit Bleephub S3 region = %q, want eu-west-1", got)
	}
	t.Setenv("BLEEPHUB_S3_REGION", "")
	if got := bleephubS3Region(); got != "us-east-1" {
		t.Fatalf("AWS S3 region = %q, want us-east-1", got)
	}
	t.Setenv("AWS_REGION", "")
	if got := bleephubS3Region(); got != "us-east-1" {
		t.Fatalf("default S3 region = %q, want us-east-1", got)
	}
}
