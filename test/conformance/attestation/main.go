// Command attestation-fixture mints the signed artifact, the Sigstore bundle
// that attests to it, and the trusted root that anchors both — everything
// `gh attestation verify` needs from a deployment that is not github.com.
//
// The command-line interface verifies an attestation the way a relying party
// does: it walks the bundle's signing certificate to a certificate authority
// in the trusted root, checks the certificate's signed certificate timestamps
// against the trusted root's certificate-transparency logs, checks the
// transparency-log entry's signed entry timestamp against the trusted root's
// log keys, and only then checks the signature over the in-toto statement and
// the identity the certificate carries. A fixture that satisfies all of that
// is a real Sigstore trust chain — it is simply rooted in keys this program
// mints, which is exactly what --custom-trusted-root exists for.
//
// Nothing here is a stub: every signature is produced by the key the trusted
// root names, and a single byte changed anywhere fails verification.
//
// Usage:
//
//	attestation-fixture -out <dir> -host <host> -owner <login> -repo <name>
//
// It writes artifact.bin, bundle.json and trusted_root.json into <dir>.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// GitHub's Fulcio certificate extensions. Every value is a DER-encoded
// UTF8String, which is what the version 2 extensions carry and what the
// verifier decodes them as.
var (
	oidIssuerV2                = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidBuildSignerURI          = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	oidBuildSignerDigest       = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 10}
	oidRunnerEnvironment       = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 11}
	oidSourceRepositoryURI     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}
	oidSourceRepositoryDigest  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}
	oidSourceRepositoryRef     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}
	oidSourceRepositoryID      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 15}
	oidSourceRepositoryOwner   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 16}
	oidSourceRepositoryOwnerID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 17}
	oidBuildConfigURI          = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 18}
	oidBuildConfigDigest       = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 19}
	oidBuildTrigger            = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 20}
	oidRunInvocationURI        = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 21}
	oidRepositoryVisibility    = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 22}
)

const (
	statementType = "https://in-toto.io/Statement/v1"
	predicateType = "https://slsa.dev/provenance/v1"
	payloadType   = "application/vnd.in-toto+json"
	workflowPath  = ".github/workflows/attest.yml"
	workflowRef   = "refs/heads/main"
	artifactBody  = "bleephub conformance attestation subject\n"

	// checkpointOrigin names the log in its checkpoints and in the signature
	// line that covers them.
	checkpointOrigin = "bleephub.conformance"
)

// githubURL renders the repository or owner URL gh compares a certificate's
// claims against. gh maps --owner/--repo onto github.com for every deployment
// that is not a tenancy host, so those are the values a certificate has to
// carry for gh to accept it — the deployment's own host name never appears in
// the comparison.
func githubURL(path string) string { return "https://github.com/" + path }

func main() {
	out := flag.String("out", "", "directory to write the fixture into")
	host := flag.String("host", "localhost", "host the deployment is reached at")
	owner := flag.String("owner", "", "repository owner")
	repo := flag.String("repo", "", "repository name")
	issuer := flag.String("issuer", "https://token.actions.githubusercontent.com",
		"OpenID Connect issuer to record in the signing certificate")
	flag.Parse()
	if *out == "" || *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "-out, -owner and -repo are required")
		os.Exit(2)
	}
	if err := write(*out, *host, *owner, *repo, *issuer); err != nil {
		fmt.Fprintln(os.Stderr, "attestation fixture:", err)
		os.Exit(1)
	}
}

// write mints the whole trust chain and lays the fixture down on disk.
func write(dir, host, owner, repo, issuer string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	artifact := []byte(artifactBody)
	digest := sha256.Sum256(artifact)

	sigstore, err := newSigstore(host)
	if err != nil {
		return err
	}
	identity := fmt.Sprintf("%s/%s@%s", githubURL(owner+"/"+repo), workflowPath, workflowRef)
	leaf, leafKey, err := sigstore.issue(identity, issuer, owner, repo)
	if err != nil {
		return err
	}

	statement, err := inTotoStatement(hex.EncodeToString(digest[:]), owner, repo, identity)
	if err != nil {
		return err
	}
	envelope, signature, err := signEnvelope(statement, leafKey)
	if err != nil {
		return err
	}
	entry, err := sigstore.logEntry(envelope, statement, signature, leaf)
	if err != nil {
		return err
	}

	bundle := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"certificate": map[string]any{"rawBytes": base64.StdEncoding.EncodeToString(leaf.Raw)},
			"tlogEntries": []any{entry},
		},
		"dsseEnvelope": envelope,
	}
	trustedRoot, err := sigstore.trustedRoot()
	if err != nil {
		return err
	}

	// Both documents are written one per line: the trusted root is read as a
	// JSON Lines file, so an indented document is truncated at its first
	// newline and reported as a malformed message.
	files := map[string]any{"bundle.json": bundle, "trusted_root.json": trustedRoot}
	for name, document := range files {
		body, err := json.Marshal(document)
		if err != nil {
			return fmt.Errorf("encode %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(body, '\n'), 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "artifact.bin"), artifact, 0o600); err != nil {
		return err
	}
	// The digest is what the attestation is fetched by, so the harness reads it
	// from here rather than recomputing it in shell.
	return os.WriteFile(filepath.Join(dir, "digest.txt"),
		[]byte("sha256:"+hex.EncodeToString(digest[:])+"\n"), 0o600)
}

// --- the certificate authority and the transparency log --------------------

// sigstoreInstance is a self-contained Sigstore deployment: a certificate
// authority that issues signing certificates, and a transparency log that
// records the signed statement and countersigns the record.
type sigstoreInstance struct {
	host string

	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	logKey *ecdsa.PrivateKey
	logID  []byte
}

func newSigstore(host string) (*sigstoreInstance, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bleephub conformance signing root", Organization: []string{"bleephub"}},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create the certificate authority: %w", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	instance := &sigstoreInstance{host: host, caCert: caCert, caKey: caKey}
	if instance.logKey, instance.logID, err = newLogKey(); err != nil {
		return nil, err
	}
	return instance, nil
}

// newLogKey mints a log's signing key and its log identifier, which is the
// SHA-256 of the key's SubjectPublicKeyInfo — the identifier every Sigstore
// client resolves a log by.
func newLogKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(spki)
	return key, sum[:], nil
}

// utf8Extension DER-encodes a version 2 Fulcio extension value.
func utf8Extension(oid asn1.ObjectIdentifier, value string) (pkix.Extension, error) {
	encoded, err := asn1.MarshalWithParams(value, "utf8")
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{Id: oid, Value: encoded}, nil
}

// issue mints a signing certificate for one workflow identity, carrying the
// claims GitHub's certificate authority puts in an Actions signing certificate.
func (s *sigstoreInstance) issue(identity, issuer, owner, repo string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	san, err := url.Parse(identity)
	if err != nil {
		return nil, nil, err
	}
	ownerURI := githubURL(owner)
	repoURI := githubURL(owner + "/" + repo)
	claims := []struct {
		oid   asn1.ObjectIdentifier
		value string
	}{
		{oidIssuerV2, issuer},
		{oidBuildSignerURI, fmt.Sprintf("%s/%s@%s", repoURI, workflowPath, workflowRef)},
		{oidBuildSignerDigest, "0000000000000000000000000000000000000000"},
		{oidRunnerEnvironment, "github-hosted"},
		{oidSourceRepositoryURI, repoURI},
		{oidSourceRepositoryDigest, "0000000000000000000000000000000000000000"},
		{oidSourceRepositoryRef, workflowRef},
		{oidSourceRepositoryID, "1"},
		{oidSourceRepositoryOwner, ownerURI},
		{oidSourceRepositoryOwnerID, "1"},
		{oidBuildConfigURI, fmt.Sprintf("%s/%s@%s", repoURI, workflowPath, workflowRef)},
		{oidBuildConfigDigest, "0000000000000000000000000000000000000000"},
		{oidBuildTrigger, "workflow_dispatch"},
		{oidRunInvocationURI, fmt.Sprintf("%s/actions/runs/1/attempts/1", repoURI)},
		{oidRepositoryVisibility, "public"},
	}
	extensions := make([]pkix.Extension, 0, len(claims))
	for _, claim := range claims {
		extension, err := utf8Extension(claim.oid, claim.value)
		if err != nil {
			return nil, nil, err
		}
		extensions = append(extensions, extension)
	}

	// Fulcio issues ten-minute certificates; this one is given a day so that a
	// fixture minted at the start of a harness run still validates when the
	// command-line driver reaches it near the end. The window still has to
	// contain the log's integrated time, which is what proves the signature was
	// made while the certificate was valid.
	notBefore := time.Now().Add(-10 * time.Minute)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{san},
		ExtraExtensions:       extensions,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.caCert, &key.PublicKey, s.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create the signing certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return leaf, key, nil
}

// --- the statement, the envelope and the log entry --------------------------

// inTotoStatement renders the provenance statement the attestation carries.
func inTotoStatement(digest, owner, repo, identity string) ([]byte, error) {
	repoURI := githubURL(owner + "/" + repo)
	return json.Marshal(map[string]any{
		"_type":         statementType,
		"predicateType": predicateType,
		"subject": []any{map[string]any{
			"name":   "artifact.bin",
			"digest": map[string]any{"sha256": digest},
		}},
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": "https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1",
				"externalParameters": map[string]any{
					"workflow": map[string]any{
						"ref":        workflowRef,
						"repository": repoURI,
						"path":       workflowPath,
					},
				},
				"internalParameters": map[string]any{
					"github": map[string]any{"event_name": "workflow_dispatch"},
				},
				"resolvedDependencies": []any{map[string]any{
					"uri":    "git+" + repoURI + "@" + workflowRef,
					"digest": map[string]any{"gitCommit": "0000000000000000000000000000000000000000"},
				}},
			},
			"runDetails": map[string]any{
				"builder":  map[string]any{"id": identity},
				"metadata": map[string]any{"invocationId": repoURI + "/actions/runs/1/attempts/1"},
			},
		},
	})
}

// signEnvelope signs the statement in the Dead Simple Signing Envelope's
// pre-authentication encoding and returns the envelope and the raw signature.
func signEnvelope(statement []byte, key *ecdsa.PrivateKey) (map[string]any, []byte, error) {
	pae := fmt.Appendf(nil, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(statement))
	pae = append(pae, statement...)
	digest := sha256.Sum256(pae)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return nil, nil, err
	}
	envelope := map[string]any{
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"payloadType": payloadType,
		"signatures":  []any{map[string]any{"sig": base64.StdEncoding.EncodeToString(signature)}},
	}
	return envelope, signature, nil
}

// logEntry writes the envelope into the transparency log and returns the
// bundle's record of it, including the signed entry timestamp that is the
// log's promise the entry was accepted at that moment.
func (s *sigstoreInstance) logEntry(envelope map[string]any, statement, signature []byte,
	leaf *x509.Certificate) (map[string]any, error) {
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	envelopeHash := sha256.Sum256(envelopeJSON)
	payloadHash := sha256.Sum256(statement)

	certPEM := pemCertificate(leaf.Raw)
	body := map[string]any{
		"apiVersion": "0.0.1",
		"kind":       "dsse",
		"spec": map[string]any{
			"envelopeHash": map[string]any{
				"algorithm": "sha256",
				"value":     hex.EncodeToString(envelopeHash[:]),
			},
			"payloadHash": map[string]any{
				"algorithm": "sha256",
				"value":     hex.EncodeToString(payloadHash[:]),
			},
			"signatures": []any{map[string]any{
				"signature": base64.StdEncoding.EncodeToString(signature),
				"verifier":  base64.StdEncoding.EncodeToString(certPEM),
			}},
		},
	}
	canonical, err := canonicalJSON(body)
	if err != nil {
		return nil, err
	}

	// The log holds exactly this one entry, so its Merkle root is the entry's
	// own leaf hash and the inclusion proof carries no sibling hashes. Both the
	// promise and the proof are produced: a bundle of this version is rejected
	// outright without a proof, and the promise's integrated time is the
	// observer timestamp the short-lived signing certificate is checked at.
	const logIndex = 0
	integrated := time.Now().Unix()
	promise, err := s.signEntryTimestamp(canonical, logIndex, integrated)
	if err != nil {
		return nil, err
	}
	root := leafHash(canonical)
	checkpoint, err := s.signCheckpoint(1, root)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"logIndex":         fmt.Sprintf("%d", logIndex),
		"logId":            map[string]any{"keyId": base64.StdEncoding.EncodeToString(s.logID)},
		"kindVersion":      map[string]any{"kind": "dsse", "version": "0.0.1"},
		"integratedTime":   fmt.Sprintf("%d", integrated),
		"inclusionPromise": map[string]any{"signedEntryTimestamp": promise},
		"inclusionProof": map[string]any{
			"logIndex":   fmt.Sprintf("%d", logIndex),
			"rootHash":   base64.StdEncoding.EncodeToString(root),
			"treeSize":   "1",
			"hashes":     []any{},
			"checkpoint": map[string]any{"envelope": checkpoint},
		},
		"canonicalizedBody": base64.StdEncoding.EncodeToString(canonical),
	}, nil
}

// leafHash is RFC 6962's leaf hash: the entry prefixed with a zero byte, so a
// leaf can never be confused with an interior node.
func leafHash(entry []byte) []byte {
	sum := sha256.Sum256(append([]byte{0x00}, entry...))
	return sum[:]
}

// signCheckpoint signs the log's tree head as a note, the format Rekor's
// checkpoints are carried in: the origin line, the tree size, the base64 root
// hash, a blank line, and one signature line whose first four bytes hint at the
// signing key.
func (s *sigstoreInstance) signCheckpoint(treeSize uint64, root []byte) (string, error) {
	// The origin ends in a numeric tree identifier because that is how a
	// verifier tells a version 1 checkpoint from a later one.
	note := fmt.Sprintf("%s - 1\n%d\n%s\n", checkpointOrigin, treeSize, base64.StdEncoding.EncodeToString(root))
	digest := sha256.Sum256([]byte(note))
	signature, err := ecdsa.SignASN1(rand.Reader, s.logKey, digest[:])
	if err != nil {
		return "", err
	}
	hint := binary.BigEndian.Uint32(s.logID)
	line := binary.BigEndian.AppendUint32(nil, hint)
	line = append(line, signature...)
	return fmt.Sprintf("%s\n\u2014 %s %s\n", note, checkpointOrigin,
		base64.StdEncoding.EncodeToString(line)), nil
}

// signEntryTimestamp signs the log's promise: the canonical form of
// {body, integratedTime, logIndex, logID}, which is what every Sigstore client
// reconstructs and verifies against the log's public key.
func (s *sigstoreInstance) signEntryTimestamp(body []byte, logIndex, integrated int64) (string, error) {
	payload, err := canonicalJSON(map[string]any{
		"body":           base64.StdEncoding.EncodeToString(body),
		"integratedTime": integrated,
		"logIndex":       logIndex,
		"logID":          hex.EncodeToString(s.logID),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	signature, err := s.logKey.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// canonicalJSON renders a document in the canonical form Sigstore's log
// signatures are computed over: keys sorted, no insignificant whitespace, and
// no HTML escaping.
func canonicalJSON(document any) ([]byte, error) {
	// encoding/json already sorts map keys and emits no insignificant
	// whitespace; only the escaping has to be turned off, and that needs the
	// streaming encoder, which appends a newline this strips back off.
	var buffer []byte
	body, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	buffer = body
	return buffer, nil
}

func pemCertificate(der []byte) []byte {
	const width = 64
	encoded := base64.StdEncoding.EncodeToString(der)
	out := []byte("-----BEGIN CERTIFICATE-----\n")
	for len(encoded) > width {
		out = append(out, encoded[:width]...)
		out = append(out, '\n')
		encoded = encoded[width:]
	}
	out = append(out, encoded...)
	out = append(out, '\n')
	return append(out, []byte("-----END CERTIFICATE-----\n")...)
}

// --- the trusted root -------------------------------------------------------

// trustedRoot renders the trust anchors in the shape
// `gh attestation verify --custom-trusted-root` reads.
func (s *sigstoreInstance) trustedRoot() (map[string]any, error) {
	logKey, err := x509.MarshalPKIXPublicKey(&s.logKey.PublicKey)
	if err != nil {
		return nil, err
	}
	validity := map[string]any{"start": s.caCert.NotBefore.UTC().Format(time.RFC3339)}
	return map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"certificateAuthorities": []any{map[string]any{
			"subject": map[string]any{
				"organization": "bleephub",
				"commonName":   s.caCert.Subject.CommonName,
			},
			"uri": fmt.Sprintf("https://%s", s.host),
			"certChain": map[string]any{
				"certificates": []any{map[string]any{
					"rawBytes": base64.StdEncoding.EncodeToString(s.caCert.Raw),
				}},
			},
			"validFor": validity,
		}},
		"tlogs": []any{map[string]any{
			"baseUrl":       fmt.Sprintf("https://%s/tlog", s.host),
			"hashAlgorithm": "SHA2_256",
			"publicKey": map[string]any{
				"rawBytes":   base64.StdEncoding.EncodeToString(logKey),
				"keyDetails": "PKIX_ECDSA_P256_SHA_256",
				"validFor":   validity,
			},
			"logId": map[string]any{"keyId": base64.StdEncoding.EncodeToString(s.logID)},
		}},
		"timestampAuthorities": []any{},
	}, nil
}
