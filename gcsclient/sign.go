package gcsclient

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// signingAlgorithm names a V4 signature made with a service account's RSA key.
	signingAlgorithm = "GOOG4-RSA-SHA256"
	// MaximumSignedURLExpiry is the longest a V4 signed URL can be made to last.
	// https://docs.cloud.google.com/storage/docs/authentication/canonical-requests#required-query-parameters
	MaximumSignedURLExpiry = 7 * 24 * time.Hour
)

// SignedGetURL returns a URL that reads the object, by GET and with no
// credentials, from now until expiry has passed. The expiry is carried in whole
// seconds, so it must be at least one, and what is left of a second is dropped;
// the service signs for no longer than MaximumSignedURLExpiry.
//
// The URL is signed here, with the service account's private key, and nothing
// is sent: a URL for an object that does not exist is signed as readily as any
// other, and is answered 404 when it is used. It is path-style — the bucket is
// the first segment of the path, under the client's endpoint — which is the one
// form that also addresses an emulator.
//
// This is the V4 signing process, as documented:
// https://docs.cloud.google.com/storage/docs/access-control/signing-urls-manually
// https://docs.cloud.google.com/storage/docs/authentication/canonical-requests
// https://docs.cloud.google.com/storage/docs/authentication/signatures
func (c *Client) SignedGetURL(bucket, name string, expiry time.Duration) (string, error) {
	what := named(bucket, name)
	if expiry < time.Second || expiry > MaximumSignedURLExpiry {
		return "", fmt.Errorf("gcs sign %s: expiry %s is not between one second and the %s the service allows", what, expiry, MaximumSignedURLExpiry)
	}
	if bucket == "" || name == "" {
		return "", fmt.Errorf("gcs sign %s: a bucket and an object must both be named", what)
	}
	now := c.clock().UTC()
	timestamp := now.Format("20060102T150405Z")
	// The location of a credential scope may be "auto" for Cloud Storage, which
	// keeps a signer from having to know where a bucket is.
	credentialScope := now.Format("20060102") + "/auto/storage/goog4_request"

	// "/" separates the segments of an object's name in a path and is left as it
	// is; every other byte outside the unreserved set is encoded, which covers
	// the characters the service requires to be and is what Google's own samples
	// do.
	path := "/" + percentEncode(bucket, false) + "/" + percentEncode(name, true)
	// The parameters are in order of their names by code point already, which the
	// canonical form requires, and their values are encoded as the path is.
	query := strings.Join([]string{
		"X-Goog-Algorithm=" + signingAlgorithm,
		"X-Goog-Credential=" + percentEncode(c.email+"/"+credentialScope, false),
		"X-Goog-Date=" + timestamp,
		"X-Goog-Expires=" + strconv.FormatInt(int64(expiry/time.Second), 10),
		"X-Goog-SignedHeaders=host",
	}, "&")
	// Host is the one header signed, and the one header every GET carries: a URL
	// that signed more could be fetched only by a caller who knew to send them.
	canonicalRequest := strings.Join([]string{
		"GET",
		path,
		query,
		"host:" + c.host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	hashed := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{signingAlgorithm, timestamp, credentialScope, hex.EncodeToString(hashed[:])}, "\n")
	digest := sha256.Sum256([]byte(stringToSign))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("gcs sign %s: %w", what, err)
	}
	return c.endpoint + path + "?" + query + "&X-Goog-Signature=" + hex.EncodeToString(signature), nil
}
