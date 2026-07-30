package dqliteaddr

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"time"
)

const dqliteTLSName = "bleephub-dqlite.internal"

// TLSConfig derives the private-cluster TLS identity from the independently
// generated dqlite cluster secret. Every voter and Bleephub task already needs
// that secret to authenticate the HTTP upgrade; deriving the transport
// identity from it adds encryption and server authentication without creating
// a second certificate-distribution and rotation system that could drift from
// cluster membership.
//
// Rotating BLEEPHUB_DQLITE_SECRET rotates both the upgrade credential and the
// TLS identity. The certificate validity is intentionally broad because secret
// rotation, not wall-clock renewal, is the trust boundary.
func TLSConfig(secret string, server bool) (*tls.Config, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("dqlite TLS requires the cluster secret")
	}
	seed := sha256.Sum256([]byte("bleephub:dqlite:tls:v1\x00" + secret))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	serialBytes := sha256.Sum256([]byte("bleephub:dqlite:serial:v1\x00" + secret))
	serialBytes[0] &= 0x7f
	if binary.BigEndian.Uint64(serialBytes[:8]) == 0 {
		serialBytes[7] = 1
	}
	template := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(serialBytes[:20]),
		Subject:               pkix.Name{CommonName: dqliteTLSName},
		DNSNames:              []string{dqliteTLSName},
		NotBefore:             time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2120, time.January, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, err
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	roots.AddCert(parsed)

	config := &tls.Config{MinVersion: tls.VersionTLS13}
	if server {
		config.Certificates = []tls.Certificate{certificate}
	} else {
		config.RootCAs = roots
		config.ServerName = dqliteTLSName
	}
	return config, nil
}
