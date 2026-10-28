package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

// DefaultNodeValidity is how long an issued node certificate stays valid. Node
// identities are long-lived infrastructure, so this mirrors the install-time
// chain (10 years) and keeps renewal out of the critical path.
const DefaultNodeValidity = 10 * 365 * 24 * time.Hour

// IssuedServer is a leaf certificate plus the metadata the panel persists so it
// can pin the identity on later dials and revoke it individually.
type IssuedServer struct {
	CertPEM     []byte
	KeyPEM      []byte
	Serial      string
	Fingerprint string // SHA-256 over the DER encoding, uppercase hex
	NotAfter    time.Time
}

// IssueServerFromCA signs a server certificate for cn with the given SANs using
// the operator's CA key. SEC1 ("EC PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") EC
// keys are both accepted, because cmd/gencerts and openssl write different
// encodings.
func IssueServerFromCA(caCertPEM, caKeyPEM []byte, cn string, sans []string, validity time.Duration) (*IssuedServer, error) {
	if cn == "" {
		return nil, fmt.Errorf("certificate CN is required")
	}
	caCert, err := ParseCertificate(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	caKey, err := parseECKey(caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	if validity <= 0 {
		validity = DefaultNodeValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	dns, ips := splitSANs(sans)
	notAfter := time.Now().Add(validity)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &IssuedServer{
		CertPEM:     encodeCert(der),
		KeyPEM:      keyPEM,
		Serial:      tmpl.SerialNumber.Text(16),
		Fingerprint: strings.ToUpper(hex.EncodeToString(sum[:])),
		NotAfter:    notAfter,
	}, nil
}

// ParseCertificate decodes the first CERTIFICATE block in pemBytes.
func ParseCertificate(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no PEM CERTIFICATE block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// FingerprintPEM returns the SHA-256 fingerprint of a certificate's DER
// encoding (uppercase hex), matching what IssueServerFromCA records.
func FingerprintPEM(certPEM []byte) (string, error) {
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func parseECKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM key block found")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unsupported EC key encoding: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not ECDSA (got %T)", parsed)
	}
	return key, nil
}
