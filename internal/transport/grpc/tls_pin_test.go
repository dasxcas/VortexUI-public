package grpc

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/vortexui/vortexui/internal/pki"
)

func poolFrom(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA to pool")
	}
	return pool
}

func peerState(t *testing.T, leafPEM []byte, extra ...[]byte) tls.ConnectionState {
	t.Helper()
	leaf, err := pki.ParseCertificate(leafPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	certs := []*x509.Certificate{leaf}
	for _, pemBytes := range extra {
		c, err := pki.ParseCertificate(pemBytes)
		if err != nil {
			t.Fatalf("parse chain cert: %v", err)
		}
		certs = append(certs, c)
	}
	return tls.ConnectionState{PeerCertificates: certs}
}

// TestVerifyNodePeer covers the decisions the dialer makes: chain trust, address
// binding, and identity pinning. Address and fingerprint checks are what stop one
// node's certificate from standing in for another.
func TestVerifyNodePeer(t *testing.T) {
	chain, err := pki.Generate(nil)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	issued, err := pki.IssueServerFromCA(chain.CA.CertPEM, chain.CA.KeyPEM, "node-a", []string{"10.0.0.5"}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cs := peerState(t, issued.CertPEM, chain.CA.CertPEM)
	pool := poolFrom(t, chain.CA.CertPEM)

	cases := []struct {
		name        string
		serverName  string
		fingerprint string
		wantErr     bool
	}{
		{"legacy: chain only", "", "", false},
		{"pinned: address and fingerprint match", "10.0.0.5", issued.Fingerprint, false},
		{"pinned: address mismatch", "10.0.0.6", issued.Fingerprint, true},
		{"pinned: wrong fingerprint", "10.0.0.5", "AABBCCDD", true},
		{"pinned: no address given still pins", "", issued.Fingerprint, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyNodePeer(cs, pool, tc.serverName, tc.fingerprint)
			if tc.wantErr && err == nil {
				t.Fatal("expected rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

// TestVerifyNodePeerRejectsForeignCA proves a leaf from another CA is refused by
// the panel's trust pool even when its fingerprint matches what was presented.
func TestVerifyNodePeerRejectsForeignCA(t *testing.T) {
	trusted, err := pki.Generate(nil)
	if err != nil {
		t.Fatalf("generate trusted CA: %v", err)
	}
	foreign, err := pki.Generate(nil)
	if err != nil {
		t.Fatalf("generate foreign CA: %v", err)
	}
	leaf, err := pki.IssueServerFromCA(foreign.CA.CertPEM, foreign.CA.KeyPEM, "node-x", []string{"10.0.0.9"}, 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cs := peerState(t, leaf.CertPEM, foreign.CA.CertPEM)
	pool := poolFrom(t, trusted.CA.CertPEM)

	if err := verifyNodePeer(cs, pool, "10.0.0.9", leaf.Fingerprint); err == nil {
		t.Fatal("foreign CA chain should be rejected")
	}
}

// TestVerifyNodePeerNoCertificate covers the empty-peer case.
func TestVerifyNodePeerNoCertificate(t *testing.T) {
	pool := x509.NewCertPool()
	if err := verifyNodePeer(tls.ConnectionState{}, pool, "10.0.0.5", "AA"); err == nil {
		t.Fatal("empty peer chain should be rejected")
	}
}
