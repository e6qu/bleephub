package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fixture is only worth anything if every signature in it verifies, and the
// client that checks that lives in a container behind a Docker build. These
// tests re-verify the whole chain here, with the same reconstructions the
// Sigstore verifier performs, so a change that silently breaks the fixture
// fails in the Go suite in milliseconds rather than in the conformance run.

const (
	testOwner = "conformance"
	testRepo  = "conformance-gh"
)

type fixture struct {
	bundle      map[string]any
	trustedRoot map[string]any
	artifact    []byte
	digest      string
}

func writeFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	if err := write(dir, "localhost", testOwner, testRepo, "https://token.actions.githubusercontent.com"); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	read := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return body
	}
	loaded := fixture{artifact: read("artifact.bin"), digest: strings.TrimSpace(string(read("digest.txt")))}
	if err := json.Unmarshal(read("bundle.json"), &loaded.bundle); err != nil {
		t.Fatalf("parse the bundle: %v", err)
	}
	if err := json.Unmarshal(read("trusted_root.json"), &loaded.trustedRoot); err != nil {
		t.Fatalf("parse the trusted root: %v", err)
	}
	return loaded
}

// dig walks a decoded JSON document. A missing or wrongly typed member is a
// test failure at the point it is read, which keeps every assertion below one
// line long.
func dig(t *testing.T, document any, path ...string) any {
	t.Helper()
	current := document
	for index, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", strings.Join(path[:index], "."))
		}
		current, ok = object[key]
		if !ok {
			t.Fatalf("%s is absent", strings.Join(path[:index+1], "."))
		}
	}
	return current
}

func digString(t *testing.T, document any, path ...string) string {
	t.Helper()
	value, ok := dig(t, document, path...).(string)
	if !ok {
		t.Fatalf("%s is not a string", strings.Join(path, "."))
	}
	return value
}

func decodeBase64(t *testing.T, what, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("%s is not base64: %v", what, err)
	}
	return decoded
}

func firstOf(t *testing.T, document any, path ...string) any {
	t.Helper()
	list, ok := dig(t, document, path...).([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%s is empty", strings.Join(path, "."))
	}
	return list[0]
}

// leafCertificate parses the signing certificate and checks it chains to the
// certificate authority the trusted root names — the first thing a verifier
// does with a bundle.
func leafCertificate(t *testing.T, f fixture) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(decodeBase64(t, "the signing certificate",
		digString(t, f.bundle, "verificationMaterial", "certificate", "rawBytes")))
	if err != nil {
		t.Fatalf("parse the signing certificate: %v", err)
	}
	authority := firstOf(t, f.trustedRoot, "certificateAuthorities")
	rootBytes := decodeBase64(t, "the certificate authority",
		digString(t, firstOf(t, authority, "certChain", "certificates"), "rawBytes"))
	root, err := x509.ParseCertificate(rootBytes)
	if err != nil {
		t.Fatalf("parse the certificate authority: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		t.Fatalf("the signing certificate does not chain to the trusted root: %v", err)
	}
	return leaf
}

func TestTheSigningCertificateCarriesTheClaimsTheClientEnforces(t *testing.T) {
	t.Parallel()
	f := writeFixture(t)
	leaf := leafCertificate(t, f)

	// gh maps --owner/--repo onto github.com whatever host the deployment is
	// reached at, and compares these three extensions case-insensitively.
	claims := map[string]string{
		oidSourceRepositoryOwner.String(): "https://github.com/" + testOwner,
		oidSourceRepositoryURI.String():   "https://github.com/" + testOwner + "/" + testRepo,
		oidIssuerV2.String():              "https://token.actions.githubusercontent.com",
	}
	for _, extension := range leaf.Extensions {
		want, checked := claims[extension.Id.String()]
		if !checked {
			continue
		}
		if got := string(extension.Value[2:]); got != want {
			t.Errorf("extension %s = %q, want %q", extension.Id, got, want)
		}
		delete(claims, extension.Id.String())
	}
	for oid := range claims {
		t.Errorf("the certificate carries no %s extension", oid)
	}

	if len(leaf.URIs) != 1 {
		t.Fatalf("subject alternative names = %v, want exactly one", leaf.URIs)
	}
	if prefix := "https://github.com/" + testOwner + "/" + testRepo + "/"; !strings.HasPrefix(leaf.URIs[0].String(), prefix) {
		t.Errorf("subject alternative name %q does not start with %q", leaf.URIs[0], prefix)
	}
	if len(leaf.Issuer.Organization) != 1 {
		t.Errorf("issuer organization = %v, want exactly one entry (the client keys its verifier on it)",
			leaf.Issuer.Organization)
	}
}

func TestTheStatementIsSignedByTheCertificateAndNamesTheArtifact(t *testing.T) {
	t.Parallel()
	f := writeFixture(t)
	leaf := leafCertificate(t, f)

	statement := decodeBase64(t, "the payload", digString(t, f.bundle, "dsseEnvelope", "payload"))
	signature := decodeBase64(t, "the signature",
		digString(t, firstOf(t, f.bundle, "dsseEnvelope", "signatures"), "sig"))

	pae := append([]byte("DSSEv1 "+strconv.Itoa(len(payloadType))+" "+payloadType+" "+
		strconv.Itoa(len(statement))+" "), statement...)
	sum := sha256.Sum256(pae)
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("the signing certificate carries a %T, not an elliptic curve key", leaf.PublicKey)
	}
	if !ecdsa.VerifyASN1(key, sum[:], signature) {
		t.Fatal("the envelope signature does not verify against the signing certificate")
	}

	var decoded map[string]any
	if err := json.Unmarshal(statement, &decoded); err != nil {
		t.Fatalf("the payload is not an in-toto statement: %v", err)
	}
	if got := digString(t, decoded, "predicateType"); got != predicateType {
		t.Errorf("predicateType = %q, want %q", got, predicateType)
	}
	artifactDigest := sha256.Sum256(f.artifact)
	subject := firstOf(t, decoded, "subject")
	if got := digString(t, subject, "digest", "sha256"); got != hex.EncodeToString(artifactDigest[:]) {
		t.Errorf("the statement's subject digest %q is not the artifact's", got)
	}
	if f.digest != "sha256:"+hex.EncodeToString(artifactDigest[:]) {
		t.Errorf("digest.txt = %q, which is not the artifact's digest", f.digest)
	}
}

func TestTheLogEntryCountersignsTheEnvelope(t *testing.T) {
	t.Parallel()
	f := writeFixture(t)
	entry := firstOf(t, f.bundle, "verificationMaterial", "tlogEntries")
	body := decodeBase64(t, "the canonicalized body", digString(t, entry, "canonicalizedBody"))

	logEntry := firstOf(t, f.trustedRoot, "tlogs")
	keyBytes := decodeBase64(t, "the log key", digString(t, logEntry, "publicKey", "rawBytes"))
	parsed, err := x509.ParsePKIXPublicKey(keyBytes)
	if err != nil {
		t.Fatalf("parse the log key: %v", err)
	}
	logKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("the log key is a %T, not an elliptic curve key", parsed)
	}
	logID := decodeBase64(t, "the log id", digString(t, entry, "logId", "keyId"))
	if declared := decodeBase64(t, "the trusted root's log id", digString(t, logEntry, "logId", "keyId")); !bytes.Equal(logID, declared) {
		t.Fatal("the entry names a log the trusted root does not")
	}

	// The signed entry timestamp covers the canonical form of
	// {body, integratedTime, logIndex, logID} — the promise a verifier
	// reconstructs and checks against the log's key.
	integrated, err := strconv.ParseInt(digString(t, entry, "integratedTime"), 10, 64)
	if err != nil {
		t.Fatalf("integratedTime is not a number: %v", err)
	}
	logIndex, err := strconv.ParseInt(digString(t, entry, "logIndex"), 10, 64)
	if err != nil {
		t.Fatalf("logIndex is not a number: %v", err)
	}
	promise, err := json.Marshal(map[string]any{
		"body":           base64.StdEncoding.EncodeToString(body),
		"integratedTime": integrated,
		"logIndex":       logIndex,
		"logID":          hex.EncodeToString(logID),
	})
	if err != nil {
		t.Fatalf("encode the promise: %v", err)
	}
	sum := sha256.Sum256(promise)
	set := decodeBase64(t, "the signed entry timestamp",
		digString(t, entry, "inclusionPromise", "signedEntryTimestamp"))
	if !ecdsa.VerifyASN1(logKey, sum[:], set) {
		t.Fatal("the signed entry timestamp does not verify against the log key")
	}

	// The integrated time has to sit inside the signing certificate's validity,
	// because that is what a short-lived certificate is checked against.
	leaf := leafCertificate(t, f)
	when := time.Unix(integrated, 0)
	if when.Before(leaf.NotBefore) || when.After(leaf.NotAfter) {
		t.Errorf("the entry's integrated time %s is outside the certificate's validity (%s..%s)",
			when, leaf.NotBefore, leaf.NotAfter)
	}
}

func TestTheCheckpointCommitsToTheEntry(t *testing.T) {
	t.Parallel()
	f := writeFixture(t)
	entry := firstOf(t, f.bundle, "verificationMaterial", "tlogEntries")
	body := decodeBase64(t, "the canonicalized body", digString(t, entry, "canonicalizedBody"))
	proof := dig(t, entry, "inclusionProof")

	// One entry: the tree's root is that entry's leaf hash and the proof
	// carries no sibling hashes.
	root := decodeBase64(t, "the root hash", digString(t, proof, "rootHash"))
	if want := leafHash(body); !bytes.Equal(root, want) {
		t.Errorf("root hash %x is not the entry's leaf hash %x", root, want)
	}
	if hashes, ok := dig(t, proof, "hashes").([]any); !ok || len(hashes) != 0 {
		t.Errorf("inclusion proof hashes = %v, want none for a tree of one", hashes)
	}
	if size := digString(t, proof, "treeSize"); size != "1" {
		t.Errorf("treeSize = %q, want 1", size)
	}

	// The checkpoint is a signed note: three lines, a blank line, then one
	// signature line whose first four bytes hint at the signing key.
	envelope := digString(t, proof, "checkpoint", "envelope")
	note, signatures, found := strings.Cut(envelope, "\n\n")
	if !found {
		t.Fatalf("the checkpoint has no signature block: %q", envelope)
	}
	note += "\n"
	lines := strings.Split(strings.TrimSuffix(note, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("checkpoint note = %q, want an origin, a size and a root hash", note)
	}
	if !strings.HasSuffix(lines[0], " - 1") {
		t.Errorf("checkpoint origin %q does not end in a tree identifier, so it reads as a later log version", lines[0])
	}
	if lines[1] != "1" {
		t.Errorf("checkpoint size = %q, want 1", lines[1])
	}
	if got := decodeBase64(t, "the checkpoint root hash", lines[2]); !bytes.Equal(got, root) {
		t.Errorf("the checkpoint commits to %x, not to the proof's root %x", got, root)
	}

	fields := strings.Fields(strings.TrimSpace(signatures))
	if len(fields) != 3 {
		t.Fatalf("checkpoint signature line = %q, want three fields", signatures)
	}
	signature := decodeBase64(t, "the checkpoint signature", fields[2])
	if len(signature) < 5 {
		t.Fatalf("the checkpoint signature is too short to carry a key hint")
	}
	logKeyBytes := decodeBase64(t, "the log key",
		digString(t, firstOf(t, f.trustedRoot, "tlogs"), "publicKey", "rawBytes"))
	logIDSum := sha256.Sum256(logKeyBytes)
	if !bytes.Equal(signature[:4], logIDSum[:4]) {
		t.Errorf("the key hint %x does not name the log key", signature[:4])
	}
	parsed, err := x509.ParsePKIXPublicKey(logKeyBytes)
	if err != nil {
		t.Fatalf("parse the log key: %v", err)
	}
	logKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("the log key is a %T, not an elliptic curve key", parsed)
	}
	sum := sha256.Sum256([]byte(note))
	if !ecdsa.VerifyASN1(logKey, sum[:], signature[4:]) {
		t.Fatal("the checkpoint signature does not verify against the log key")
	}
}

func TestTheBundleDeclaresTheVersionTheClientAccepts(t *testing.T) {
	t.Parallel()
	f := writeFixture(t)
	// gh refuses anything below v0.2, and every version from v0.2 up requires
	// an inclusion proof; both halves of that are load-bearing here.
	if got := digString(t, f.bundle, "mediaType"); got != "application/vnd.dev.sigstore.bundle.v0.3+json" {
		t.Errorf("mediaType = %q", got)
	}
	entry := firstOf(t, f.bundle, "verificationMaterial", "tlogEntries")
	dig(t, entry, "inclusionProof", "checkpoint", "envelope")
	dig(t, entry, "inclusionPromise", "signedEntryTimestamp")
	if kind := digString(t, entry, "kindVersion", "kind"); kind != "dsse" {
		t.Errorf("kindVersion.kind = %q, want dsse", kind)
	}
}
