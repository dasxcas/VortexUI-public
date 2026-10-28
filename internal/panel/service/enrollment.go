package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vortexui/vortexui/internal/domain"
)

// EnrollmentService builds node enrollment bundles from the panel's mTLS cert dir.
type EnrollmentService struct {
	CAPath string // path to ca.crt; node.crt and node.key are read from the same dir
}

// EnrollmentBundle is returned by the panel API for the node wizard.
type EnrollmentBundle struct {
	Bundle        string `json:"bundle"`
	CAFingerprint string `json:"ca_fingerprint"`
	CertDir       string `json:"cert_dir"`
}

// Bundle reads ca.crt, node.crt, node.key and returns a base64 tarball plus the CA fingerprint.
func (s *EnrollmentService) Bundle() (*EnrollmentBundle, error) {
	if s.CAPath == "" {
		return nil, errors.New("CA path not configured")
	}
	dir := filepath.Dir(s.CAPath)
	files := []string{"ca.crt", "node.crt", "node.key"}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return nil, fmt.Errorf("missing %s in %s", f, dir)
		}
	}
	fp, err := caFingerprint(s.CAPath)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: bundleFileMode(name), Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return &EnrollmentBundle{
		Bundle:        base64.StdEncoding.EncodeToString(buf.Bytes()),
		CAFingerprint: fp,
		CertDir:       dir,
	}, nil
}

// BundleFor packs a single node's certificate together with the CA. Unlike the
// legacy shared Bundle, the key here belongs to one node only.
func (s *EnrollmentService) BundleFor(cert *domain.NodeCertificate) (*EnrollmentBundle, error) {
	if cert == nil || len(cert.CertPEM) == 0 || len(cert.KeyPEM) == 0 {
		return nil, errors.New("node certificate is empty")
	}
	if s.CAPath == "" {
		return nil, errors.New("CA path not configured")
	}
	caPEM, err := os.ReadFile(s.CAPath)
	if err != nil {
		return nil, err
	}
	fp, err := caFingerprint(s.CAPath)
	if err != nil {
		return nil, err
	}
	entries := []struct {
		name string
		data []byte
	}{
		{"ca.crt", caPEM},
		{"node.crt", cert.CertPEM},
		{"node.key", cert.KeyPEM},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: bundleFileMode(e.name), Size: int64(len(e.data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return &EnrollmentBundle{
		Bundle:        base64.StdEncoding.EncodeToString(buf.Bytes()),
		CAFingerprint: fp,
		CertDir:       filepath.Dir(s.CAPath),
	}, nil
}

// bundleFileMode keeps private keys owner-only inside the tarball; the installer
// extracts with tar, so the mode survives onto the node.
func bundleFileMode(name string) int64 {
	if strings.HasSuffix(name, ".key") {
		return 0o600
	}
	return 0o644
}

func caFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("invalid PEM in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(fmt.Sprintf("%02X", b))
	}
	return strings.Join(parts, ":"), nil
}
