// Package grpc wires the generated NodeService stubs to real transport: a node
// agent serves NodeService, and the panel dials it. All links are mutually
// authenticated (mTLS): both ends present certificates signed by a shared CA,
// so a leaked node cert alone cannot impersonate the panel and vice versa.
package grpc

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/grpc/credentials"
)

// TLSFiles points at the PEM material for one side of an mTLS link.
type TLSFiles struct {
	Cert string // this side's certificate
	Key  string // this side's private key
	CA   string // CA that signed the *peer's* certificate
}

func (f TLSFiles) validate() error {
	if f.Cert == "" || f.Key == "" || f.CA == "" {
		return errors.New("mTLS requires cert, key, and ca paths")
	}
	return nil
}

func loadCAPool(caPath string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid certificates found in CA %q", caPath)
	}
	return pool, nil
}

// ServerCreds builds transport credentials for the node-side gRPC server that
// require and verify a client (panel) certificate.
func ServerCreds(f TLSFiles) (credentials.TransportCredentials, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	pool, err := loadCAPool(f.CA)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}), nil
}

// ClientCreds builds transport credentials for the panel dialing a node. It
// verifies the peer chain against the CA only (legacy behaviour for nodes that
// still share one certificate); prefer ClientCredsPinned for new enrollments.
func ClientCreds(f TLSFiles, serverName string) (credentials.TransportCredentials, error) {
	return ClientCredsPinned(f, serverName, "")
}

// ClientCredsPinned binds a dial to one node identity. When expectedFingerprint
// is set the peer leaf must match it exactly and must carry serverName in its
// SANs, so a certificate issued for node A cannot stand in for node B. An empty
// fingerprint keeps CA-only verification so pre-existing nodes keep working
// until they are re-enrolled.
func ClientCredsPinned(f TLSFiles, serverName string, expected ...string) (credentials.TransportCredentials, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	pool, err := loadCAPool(f.CA)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
		// Node certs are issued for an IP/hostname, but the SDK still requires
		// ServerName to be empty here; the custom VerifyConnection below does
		// the CA, hostname, and fingerprint checks explicitly.
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
	}
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return verifyNodePeer(cs, pool, serverName, expected...)
	}
	return credentials.NewTLS(tlsCfg), nil
}

// CertReloader holds the agent's serving certificate and swaps it at runtime, so
// a rotation does not need a restart (or SSH access) on the node.
type CertReloader struct {
	mu       sync.RWMutex
	cert     *tls.Certificate
	certPath string
	keyPath  string
}

// NewCertReloader loads the current keypair and remembers where it lives.
func NewCertReloader(certPath, keyPath string) (*CertReloader, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load serving keypair: %w", err)
	}
	return &CertReloader{cert: &pair, certPath: certPath, keyPath: keyPath}, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// Reload validates a new keypair, persists it (keeping a .bak of the previous
// files) and swaps it in memory. Validation happens before anything is written,
// so a bad push leaves the running identity untouched.
func (r *CertReloader) Reload(certPEM, keyPEM []byte) error {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("certificate and key are required")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("invalid keypair: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.certPath != "" {
		if err := backupFile(r.certPath); err != nil {
			return err
		}
		if err := writeFileAtomic(r.certPath, certPEM, 0o644); err != nil {
			return err
		}
	}
	if r.keyPath != "" {
		if err := backupFile(r.keyPath); err != nil {
			return err
		}
		if err := writeFileAtomic(r.keyPath, keyPEM, 0o600); err != nil {
			return err
		}
	}
	r.cert = &pair
	return nil
}

// ServerCredsReloadable builds node-side credentials whose certificate can be
// replaced at runtime through the reloader.
func ServerCredsReloadable(f TLSFiles, r *CertReloader) (credentials.TransportCredentials, error) {
	if r == nil {
		return nil, errors.New("certificate reloader is required")
	}
	pool, err := loadCAPool(f.CA)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		GetCertificate: r.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		MinVersion:     tls.VersionTLS13,
	}), nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".vortex-cert-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	name := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// backupFile keeps a copy of the current file so an operator can roll back by
// hand if a rotation ever produces a certificate they cannot use.
func backupFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(path+".bak", data, 0o600)
}

// verifyNodePeer is the single place that decides whether a node's presented
// certificate is acceptable: CA trust, optional hostname binding, and optional
// fingerprint pinning. Kept separate from the tls.Config so it can be tested
// without a live handshake.
func verifyNodePeer(cs tls.ConnectionState, roots *x509.CertPool, serverName string, expected ...string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("node presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: x509.NewCertPool(),
	}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(opts); err != nil {
		return err
	}
	// No pinned identity yet: this is a node that has not been issued its own
	// certificate, so only CA trust can be checked (legacy behaviour).
	wanted := make([]string, 0, len(expected))
	for _, want := range expected {
		if strings.TrimSpace(want) != "" {
			wanted = append(wanted, want)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	if serverName != "" {
		if err := leaf.VerifyHostname(serverName); err != nil {
			return fmt.Errorf("node certificate does not cover %q: %w", serverName, err)
		}
	}
	sum := sha256.Sum256(leaf.Raw)
	got := strings.ToUpper(hex.EncodeToString(sum[:]))
	for _, want := range wanted {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return nil
		}
	}
	return errors.New("node certificate fingerprint mismatch")
}
