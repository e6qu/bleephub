package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// presignProbeExpiry is how long the URL signed by the probe would last. Nothing
// fetches it.
const presignProbeExpiry = time.Minute

// Conform proves, against the live store, the guarantees gitstore is built on,
// and returns an error naming the first that does not hold. It is run once at
// startup, under a key prefix of the caller's choosing, and it cleans up after
// itself.
//
// It exists because a store can accept the requests and not honour them. Google
// Cloud Storage's S3-compatible endpoint answers a conditional PUT with 200 and
// overwrites; older releases of several S3-compatible servers did the same. A
// replica on such a store would run for weeks, and then two pushes would each be
// told they had moved a branch. There is nothing to fall back to — an object
// store that cannot arbitrate a write cannot be the only durable state of a git
// server with more than one replica — so the answer to a failed probe is not to
// start.
func Conform(ctx context.Context, bucket Bucket, prefix string) error {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("object store conformance: %w", err)
	}
	key := prefix + "conformance-" + hex.EncodeToString(nonce)
	defer func() { _ = bucket.Delete(context.WithoutCancel(ctx), key) }()

	fail := func(what string, err error) error {
		return fmt.Errorf("object store %s does not conform: %s: %w", bucket.Name(), what, err)
	}
	broken := func(what string) error {
		return fmt.Errorf("object store %s does not conform: %s", bucket.Name(), what)
	}

	first := []byte("first writer")
	created, err := bucket.Put(ctx, key, bytes.NewReader(first), int64(len(first)), IfAbsent())
	if err != nil {
		return fail("creating an object that does not exist", err)
	}
	if created == "" {
		return broken("a write returned no version")
	}

	second := []byte("second writer")
	if _, err := bucket.Put(ctx, key, bytes.NewReader(second), int64(len(second)), IfAbsent()); !errors.Is(err, ErrConditionNotMet) {
		return broken(fmt.Sprintf("a create-if-absent of an object that exists answered %v, not a refusal: two replicas would both win", err))
	}

	if err := expectContent(ctx, bucket, key, first, created); err != nil {
		return fail("reading back what the refused write must not have changed", err)
	}

	replaced, err := bucket.Put(ctx, key, bytes.NewReader(second), int64(len(second)), IfVersion(created))
	if err != nil {
		return fail("replacing an object at the version just read", err)
	}
	if replaced == created {
		return broken("an object's version did not change when its content did")
	}
	third := []byte("third writer")
	if _, err := bucket.Put(ctx, key, bytes.NewReader(third), int64(len(third)), IfVersion(created)); !errors.Is(err, ErrConditionNotMet) {
		return broken(fmt.Sprintf("a replace-if-unchanged against a stale version answered %v, not a refusal: a lost update would go unnoticed", err))
	}
	if err := expectContent(ctx, bucket, key, second, replaced); err != nil {
		return fail("reading back what the refused write must not have changed", err)
	}

	ranged, info, err := bucket.GetRange(ctx, key, 7, 3)
	if err != nil {
		return fail("a ranged read", err)
	}
	part, err := io.ReadAll(ranged)
	_ = ranged.Close()
	if err != nil {
		return fail("a ranged read", err)
	}
	if !bytes.Equal(part, second[7:10]) || info.Size != int64(len(second)) {
		return broken(fmt.Sprintf("a ranged read of bytes 7-9 returned %q and a whole size of %d, want %q and %d", part, info.Size, second[7:10], len(second)))
	}

	listed := false
	if err := bucket.List(ctx, key, func(entry Entry) error {
		listed = listed || entry.Key == key
		return nil
	}); err != nil {
		return fail("listing", err)
	}
	if !listed {
		return broken("a listing taken after a write does not include it")
	}

	if _, err := bucket.PresignGet(ctx, key, presignProbeExpiry); err != nil {
		return fail("signing a URL", err)
	}

	if err := bucket.Delete(ctx, key); err != nil {
		return fail("deleting", err)
	}
	if _, err := bucket.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		return broken(fmt.Sprintf("an object just deleted answered %v, not absence", err))
	}
	return nil
}

func expectContent(ctx context.Context, bucket Bucket, key string, want []byte, version Version) error {
	body, info, err := bucket.Get(ctx, key)
	if err != nil {
		return err
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("content is %q, want %q", got, want)
	}
	if info.Version != version {
		return fmt.Errorf("version is %q, want the %q the write returned", info.Version, version)
	}
	return nil
}
