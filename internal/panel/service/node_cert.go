package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
	"github.com/vortexui/vortexui/internal/pki"
)

// nodeCertRenewBefore re-issues a certificate that expires within this window.
const nodeCertRenewBefore = 30 * 24 * time.Hour

// NodeCertService issues and stores one mTLS identity per node. Issuance is
// idempotent: repeated enrollment requests return the stored certificate, so a
// restart, a re-sync, or a second installer run never mints a new identity.
type NodeCertService struct {
	repo      port.NodeCertRepository
	caPath    string // path to ca.crt; ca.key is read from the same directory
	log       *slog.Logger
	now       func() time.Time
	installer func(ctx context.Context, nodeID uuid.UUID, certPEM, keyPEM []byte, fingerprint string) error
}

// NewNodeCertService wires the service. caPath is the panel's CA certificate;
// the CA private key must sit next to it (that is what signs node leaves).
func NewNodeCertService(repo port.NodeCertRepository, caPath string, log *slog.Logger) *NodeCertService {
	if log == nil {
		log = slog.Default()
	}
	return &NodeCertService{repo: repo, caPath: caPath, log: log, now: time.Now}
}

// Ensure returns a usable certificate for the node, issuing one when the node
// has none, when the stored one was revoked, or when it is close to expiry.
func (s *NodeCertService) Ensure(ctx context.Context, n *domain.Node) (*domain.NodeCertificate, error) {
	if n == nil {
		return nil, errors.New("node is required")
	}
	existing, err := s.repo.Get(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if existing.Usable(s.now(), nodeCertRenewBefore) {
		return existing, nil
	}
	caCertPEM, caKeyPEM, err := s.loadCA()
	if err != nil {
		return nil, err
	}
	issued, err := pki.IssueServerFromCA(caCertPEM, caKeyPEM,
		"node-"+n.ID.String(), nodeSANs(n), pki.DefaultNodeValidity)
	if err != nil {
		return nil, fmt.Errorf("issue node certificate: %w", err)
	}
	cert := &domain.NodeCertificate{
		NodeID:      n.ID,
		Serial:      issued.Serial,
		Fingerprint: issued.Fingerprint,
		CertPEM:     issued.CertPEM,
		KeyPEM:      issued.KeyPEM,
		IssuedAt:    s.now(),
		ExpiresAt:   issued.NotAfter,
	}
	if err := s.repo.Upsert(ctx, cert); err != nil {
		return nil, err
	}
	s.log.Info("issued node certificate", "node", n.Name, "fingerprint", issued.Fingerprint, "expires", issued.NotAfter)
	return cert, nil
}

// revoke marks the node's certificate unusable. It is deliberately unexported:
// a bare revocation leaves the node with no pinned fingerprint, and the dialer
// used to treat that as "no identity yet" and fall back to CA-only checks, so
// the certificate the caller meant to kill kept connecting. Everything that
// needs to invalidate an identity goes through Rotate, which re-issues in the
// same transaction of intent.
func (s *NodeCertService) revoke(ctx context.Context, nodeID uuid.UUID) error {
	return s.repo.Revoke(ctx, nodeID)
}

// Rotate revokes the current identity and immediately issues a replacement, so
// an operator never sits in the window where the node has no pinned certificate
// (which is when the dialer falls back to CA-only verification).
func (s *NodeCertService) Rotate(ctx context.Context, n *domain.Node) (*domain.NodeCertificate, error) {
	if err := s.revoke(ctx, n.ID); err != nil {
		return nil, err
	}
	return s.Ensure(ctx, n)
}

// Fingerprint returns the pinned fingerprint for a node, if one is live.
func (s *NodeCertService) Fingerprint(ctx context.Context, nodeID uuid.UUID) (string, bool) {
	fp, ok, err := s.repo.Fingerprint(ctx, nodeID)
	if err != nil {
		s.log.Warn("node certificate fingerprint lookup failed", "node_id", nodeID, "err", err)
		return "", false
	}
	return fp, ok
}

// FPair returns the acceptable fingerprints for a node during a rotation: the
// active identity and, while a push is in flight, the pending one.
func (s *NodeCertService) FPair(ctx context.Context, nodeID uuid.UUID) (active, pending string) {
	a, p, err := s.repo.Fingerprints(ctx, nodeID)
	if err != nil {
		s.log.Warn("node certificate fingerprints lookup failed", "node_id", nodeID, "err", err)
		return "", ""
	}
	return a, p
}

// IdentityRefused reports whether the node has a stored identity that is no
// longer usable (revoked, or expired without renewal). It exists because an
// empty fingerprint used to mean "no identity at all" and the dialer fell back
// to CA-only verification — which also covered the much more dangerous case of
// an identity that had just been revoked, letting the revoked certificate keep
// connecting. Callers must refuse the dial in that state instead of downgrading.
//
// Nodes that were never enrolled return false: they are still on the legacy
// CA-only path and are migrated to pinned identities on their next enrollment.
func (s *NodeCertService) IdentityRefused(ctx context.Context, nodeID uuid.UUID) bool {
	c, err := s.repo.Get(ctx, nodeID)
	if err != nil {
		// A lookup failure must not take every node offline; the pinned check
		// still applies whenever a fingerprint is present.
		s.log.Warn("node certificate state lookup failed", "node_id", nodeID, "err", err)
		return false
	}
	return c != nil && !c.Usable(s.now(), 0)
}

// FPairPinned returns the acceptable fingerprints plus the refusal flag in one
// call, which is what the dialer needs.
func (s *NodeCertService) FPairPinned(ctx context.Context, nodeID uuid.UUID) (active, pending string, refused bool) {
	active, pending = s.FPair(ctx, nodeID)
	return active, pending, s.IdentityRefused(ctx, nodeID)
}

// SetInstaller wires the transport used to deliver a certificate to a node.
// When nil, rotation falls back to handing the operator a bundle.
func (s *NodeCertService) SetInstaller(fn func(ctx context.Context, nodeID uuid.UUID, certPEM, keyPEM []byte, fingerprint string) error) {
	s.installer = fn
}

// RotatePush issues a replacement identity, delivers it to the agent over the
// existing mTLS channel and promotes it once the agent confirms the swap. It
// returns pushed=false (with an error) when delivery is not possible, in which
// case nothing changed on either side.
func (s *NodeCertService) RotatePush(ctx context.Context, n *domain.Node) (*domain.NodeCertificate, bool, error) {
	if s.installer == nil {
		return nil, false, errors.New("certificate push is not configured")
	}
	caCertPEM, caKeyPEM, err := s.loadCA()
	if err != nil {
		return nil, false, err
	}
	issued, err := pki.IssueServerFromCA(caCertPEM, caKeyPEM,
		"node-"+n.ID.String(), nodeSANs(n), pki.DefaultNodeValidity)
	if err != nil {
		return nil, false, fmt.Errorf("issue node certificate: %w", err)
	}
	next := &domain.NodeCertificate{
		NodeID:      n.ID,
		Serial:      issued.Serial,
		Fingerprint: issued.Fingerprint,
		CertPEM:     issued.CertPEM,
		KeyPEM:      issued.KeyPEM,
		IssuedAt:    s.now(),
		ExpiresAt:   issued.NotAfter,
	}
	if err := s.repo.SetPending(ctx, n.ID, next); err != nil {
		return nil, false, err
	}
	// The RPC returns only after the agent has validated and swapped the
	// certificate, so a success here means the node already serves it.
	if err := s.installer(ctx, n.ID, issued.CertPEM, issued.KeyPEM, issued.Fingerprint); err != nil {
		_ = s.repo.ClearPending(ctx, n.ID)
		return nil, false, fmt.Errorf("push certificate to node: %w", err)
	}
	if _, err := s.repo.PromotePending(ctx, n.ID); err != nil {
		return nil, true, fmt.Errorf("certificate delivered but promotion failed: %w", err)
	}
	s.log.Info("rotated node certificate in place", "node", n.Name, "fingerprint", issued.Fingerprint)
	return next, true, nil
}

func (s *NodeCertService) loadCA() (caCertPEM, caKeyPEM []byte, err error) {
	if s.caPath == "" {
		return nil, nil, errors.New("CA path is not configured (VORTEX_TLS_CA)")
	}
	caCertPEM, err = os.ReadFile(s.caPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA certificate: %w", err)
	}
	keyPath := filepath.Join(filepath.Dir(s.caPath), "ca.key")
	caKeyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key %s (per-node issuance needs it next to ca.crt): %w", keyPath, err)
	}
	return caCertPEM, caKeyPEM, nil
}

// nodeSANs lists the names a panel may dial the node by: the address host, any
// custom endpoint, and loopback for same-host hops.
func nodeSANs(n *domain.Node) []string {
	seen := map[string]bool{}
	var sans []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		sans = append(sans, h)
	}
	add(hostOnly(n.Address))
	add(hostOnly(n.Endpoint))
	add("localhost")
	add("127.0.0.1")
	return sans
}

func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
