package grpc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/vortexui/vortexui/internal/pki"
	genv1 "github.com/vortexui/vortexui/internal/transport/genv1"
)

// TestInstallCertificateOverMTLS is the whole rotation path in one place: a
// panel-style client pushes a new identity over the existing channel, the agent
// validates and swaps it, and the next connection is judged against the new
// fingerprint while the old one is refused.
func TestInstallCertificateOverMTLS(t *testing.T) {
	chain, err := pki.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("generate chain: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	nodeCert := filepath.Join(dir, "node.crt")
	nodeKey := filepath.Join(dir, "node.key")
	panelCert := filepath.Join(dir, "panel.crt")
	panelKey := filepath.Join(dir, "panel.key")
	writePEM(t, caPath, chain.CA.CertPEM, 0o644)
	writePEM(t, nodeCert, chain.Server.CertPEM, 0o644)
	writePEM(t, nodeKey, chain.Server.KeyPEM, 0o600)
	writePEM(t, panelCert, chain.Client.CertPEM, 0o644)
	writePEM(t, panelKey, chain.Client.KeyPEM, 0o600)

	reloader, err := NewCertReloader(nodeCert, nodeKey)
	if err != nil {
		t.Fatalf("reloader: %v", err)
	}
	serverCreds, err := ServerCredsReloadable(TLSFiles{CA: caPath}, reloader)
	if err != nil {
		t.Fatalf("server creds: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(serverCreds))
	node := NewNodeServer(nil, "test", nil)
	node.SetCertReloader(reloader)
	genv1.RegisterNodeServiceServer(srv, node)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	addr := lis.Addr().String()
	fp1 := fingerprintOf(t, chain.Server.CertPEM)
	dialPinned := func(fps ...string) *NodeClient {
		t.Helper()
		creds, err := ClientCredsPinned(TLSFiles{Cert: panelCert, Key: panelKey, CA: caPath}, "127.0.0.1", fps...)
		if err != nil {
			t.Fatalf("client creds: %v", err)
		}
		cli, err := Dial(uuid.New(), addr, creds)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return cli
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	next, err := pki.IssueServerFromCA(chain.CA.CertPEM, chain.CA.KeyPEM, "node-rotated",
		[]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("issue replacement: %v", err)
	}
	fp2 := fingerprintOf(t, next.CertPEM)

	// Rejecting a mismatched fingerprint happens before anything touches disk.
	bad := dialPinned(fp1)
	if err := bad.InstallCertificate(ctx, next.CertPEM, next.KeyPEM, "0000"); err == nil {
		t.Fatal("agent must reject a fingerprint that does not match the certificate")
	}
	bad.Close()

	// The real push, over the identity the node is currently serving.
	cli := dialPinned(fp1)
	if err := cli.InstallCertificate(ctx, next.CertPEM, next.KeyPEM, fp2); err != nil {
		t.Fatalf("install certificate: %v", err)
	}
	cli.Close()

	// The old pin must now be refused, and the new one accepted.
	stale := dialPinned(fp1)
	if err := stale.InstallCertificate(ctx, next.CertPEM, next.KeyPEM, fp2); err == nil {
		t.Fatal("a client pinned to the retired fingerprint must not connect")
	}
	stale.Close()

	current := dialPinned(fp2)
	defer current.Close()
	if err := current.InstallCertificate(ctx, next.CertPEM, next.KeyPEM, fp2); err != nil {
		t.Fatalf("client pinned to the new fingerprint should connect: %v", err)
	}
	// And during a rotation window the panel accepts both at once.
	both := dialPinned(fp1, fp2)
	defer both.Close()
	if err := both.InstallCertificate(ctx, next.CertPEM, next.KeyPEM, fp2); err != nil {
		t.Fatalf("dual-accept window should connect: %v", err)
	}

	onDisk, err := os.ReadFile(nodeCert)
	if err != nil {
		t.Fatalf("read node cert: %v", err)
	}
	if fingerprintOf(t, onDisk) != fp2 {
		t.Fatal("node certificate on disk was not replaced")
	}
}
