package grpc

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vortexui/vortexui/internal/pki"
)

func writePEM(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fingerprintOf(t *testing.T, certPEM []byte) string {
	t.Helper()
	fp, err := pki.FingerprintPEM(certPEM)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// dialWithClient performs a real TLS handshake against addr using a client
// certificate from the same CA, returning the observed connection state.
func dialWithClient(t *testing.T, addr string, chain *pki.PKI, caPool *x509.CertPool) tls.ConnectionState {
	t.Helper()
	pair, err := tls.X509KeyPair(chain.Client.CertPEM, chain.Client.KeyPEM)
	if err != nil {
		t.Fatalf("client keypair: %v", err)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      caPool,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState()
}

// TestCertReloaderSwapsIdentityLive proves the node can take a new certificate
// without restarting: after Reload the very next handshake presents the new
// identity, the files on disk carry it, and the old one is kept as a backup.
func TestCertReloaderSwapsIdentityLive(t *testing.T) {
	chain, err := pki.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("generate chain: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	writePEM(t, caPath, chain.CA.CertPEM, 0o644)
	writePEM(t, certPath, chain.Server.CertPEM, 0o644)
	writePEM(t, keyPath, chain.Server.KeyPEM, 0o600)

	reloader, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}
	creds, err := ServerCredsReloadable(TLSFiles{CA: caPath}, reloader)
	if err != nil {
		t.Fatalf("server creds: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	tlsCfg := &tls.Config{
		GetCertificate: reloader.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      mustPool(t, chain.CA.CertPEM),
		MinVersion:     tls.VersionTLS13,
	}
	_ = creds // credentials wrapper is exercised in TestClientCredsPinned; raw TLS is clearer here
	srv := tls.NewListener(lis, tlsCfg)
	defer srv.Close()
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()

	pool := mustPool(t, chain.CA.CertPEM)
	fp1 := fingerprintOf(t, chain.Server.CertPEM)
	state := dialWithClient(t, lis.Addr().String(), chain, pool)
	if err := verifyNodePeer(state, pool, "127.0.0.1", fp1); err != nil {
		t.Fatalf("initial identity should verify: %v", err)
	}

	next, err := pki.IssueServerFromCA(chain.CA.CertPEM, chain.CA.KeyPEM, "node-rotated",
		[]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("issue replacement: %v", err)
	}
	fp2 := fingerprintOf(t, next.CertPEM)
	if err := reloader.Reload(next.CertPEM, next.KeyPEM); err != nil {
		t.Fatalf("reload: %v", err)
	}

	state = dialWithClient(t, lis.Addr().String(), chain, pool)
	if err := verifyNodePeer(state, pool, "127.0.0.1", fp2); err != nil {
		t.Fatalf("rotated identity should verify: %v", err)
	}
	if err := verifyNodePeer(state, pool, "127.0.0.1", fp1); err == nil {
		t.Fatal("old fingerprint must no longer match")
	}
	// During a rotation the panel accepts both, so a straggling dial cannot fail.
	if err := verifyNodePeer(state, pool, "127.0.0.1", fp1, fp2); err != nil {
		t.Fatalf("dual-accept window should verify: %v", err)
	}

	onDisk, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if fingerprintOf(t, onDisk) != fp2 {
		t.Fatal("certificate on disk was not replaced")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions = %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(keyPath + ".bak"); err != nil {
		t.Fatalf("expected a backup of the previous key: %v", err)
	}

	// A mismatched key must be rejected before anything is written, so a bad
	// push can never leave the agent unable to start.
	badCert, err := pki.IssueServerFromCA(chain.CA.CertPEM, chain.CA.KeyPEM, "other",
		[]string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("issue bad pair: %v", err)
	}
	if err := reloader.Reload(badCert.CertPEM, next.KeyPEM); err == nil {
		t.Fatal("expected mismatch between certificate and key to be rejected")
	}
	kept, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if fingerprintOf(t, kept) != fp2 {
		t.Fatal("rejected reload must not touch the installed certificate")
	}
}

func mustPool(t *testing.T, certPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append CA to pool")
	}
	return pool
}
