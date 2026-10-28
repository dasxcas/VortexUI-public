package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/pki"
)

var errPushFailed = errors.New("node unreachable")

type fakeNodeCertRepo struct {
	byNode  map[uuid.UUID]*domain.NodeCertificate
	upserts int
}

func newFakeNodeCertRepo() *fakeNodeCertRepo {
	return &fakeNodeCertRepo{byNode: map[uuid.UUID]*domain.NodeCertificate{}}
}

func (f *fakeNodeCertRepo) Get(_ context.Context, id uuid.UUID) (*domain.NodeCertificate, error) {
	return f.byNode[id], nil
}

func (f *fakeNodeCertRepo) Upsert(_ context.Context, c *domain.NodeCertificate) error {
	f.upserts++
	cp := *c
	f.byNode[c.NodeID] = &cp
	return nil
}

func (f *fakeNodeCertRepo) Revoke(_ context.Context, id uuid.UUID) error {
	if c, ok := f.byNode[id]; ok {
		now := c.IssuedAt
		c.RevokedAt = &now
	}
	return nil
}

func (f *fakeNodeCertRepo) Fingerprint(_ context.Context, id uuid.UUID) (string, bool, error) {
	c, ok := f.byNode[id]
	if !ok || c.RevokedAt != nil {
		return "", false, nil
	}
	return c.Fingerprint, true, nil
}

func (f *fakeNodeCertRepo) SetPending(_ context.Context, id uuid.UUID, c *domain.NodeCertificate) error {
	if cur, ok := f.byNode[id]; ok {
		cp := *c
		cur.Pending = &cp
	}
	return nil
}

func (f *fakeNodeCertRepo) PromotePending(_ context.Context, id uuid.UUID) (bool, error) {
	cur, ok := f.byNode[id]
	if !ok || cur.Pending == nil {
		return false, nil
	}
	p := cur.Pending
	cur.Fingerprint = p.Fingerprint
	cur.CertPEM = p.CertPEM
	cur.KeyPEM = p.KeyPEM
	cur.Serial = p.Serial
	cur.IssuedAt = p.IssuedAt
	cur.ExpiresAt = p.ExpiresAt
	cur.RevokedAt = nil
	cur.Pending = nil
	return true, nil
}

func (f *fakeNodeCertRepo) ClearPending(_ context.Context, id uuid.UUID) error {
	if cur, ok := f.byNode[id]; ok {
		cur.Pending = nil
	}
	return nil
}

func (f *fakeNodeCertRepo) Fingerprints(_ context.Context, id uuid.UUID) (string, string, error) {
	c, ok := f.byNode[id]
	if !ok || c.RevokedAt != nil {
		return "", "", nil
	}
	pending := ""
	if c.Pending != nil {
		pending = c.Pending.Fingerprint
	}
	return c.Fingerprint, pending, nil
}

func writeCA(t *testing.T) string {
	t.Helper()
	chain, err := pki.Generate(nil)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, chain.CA.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), chain.CA.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return caPath
}

// TestNodeCertServiceEnsureIsIdempotent proves re-enrolling a node never mints a
// second identity — that is what keeps issuance cheap and audits clean.
func TestNodeCertServiceEnsureIsIdempotent(t *testing.T) {
	repo := newFakeNodeCertRepo()
	svc := NewNodeCertService(repo, writeCA(t), nil)
	node := &domain.Node{ID: uuid.New(), Name: "n1", Address: "10.0.0.5:50051"}

	first, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint changed on re-enroll: %s -> %s", first.Fingerprint, second.Fingerprint)
	}
	if repo.upserts != 1 {
		t.Fatalf("expected exactly 1 issuance, got %d", repo.upserts)
	}
	if fp, ok := svc.Fingerprint(context.Background(), node.ID); !ok || fp != first.Fingerprint {
		t.Fatalf("Fingerprint() = %q,%v want %q,true", fp, ok, first.Fingerprint)
	}
}

// TestNodeCertServiceRevokeRotates proves revocation actually invalidates the
// identity, that a bare revocation is refused by the dialer, and that the next
// enrollment issues a replacement.
func TestNodeCertServiceRevokeRotates(t *testing.T) {
	repo := newFakeNodeCertRepo()
	svc := NewNodeCertService(repo, writeCA(t), nil)
	node := &domain.Node{ID: uuid.New(), Name: "n2", Address: "node.example.com:50051"}

	first, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Reach into the repo: the service no longer exposes a bare Revoke, which
	// is the point. This reproduces the state an old caller could create.
	if err := repo.Revoke(context.Background(), node.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := svc.Fingerprint(context.Background(), node.ID); ok {
		t.Fatal("revoked certificate must not be pinned any more")
	}
	if !svc.IdentityRefused(context.Background(), node.ID) {
		t.Fatal("a revoked identity must be refused instead of falling back to CA-only")
	}
	if a, p, refused := svc.FPairPinned(context.Background(), node.ID); a != "" || p != "" || !refused {
		t.Fatalf("FPairPinned = (%q,%q,%v), want empty and refused", a, p, refused)
	}
	second, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("Ensure after revoke: %v", err)
	}
	if second.Fingerprint == first.Fingerprint {
		t.Fatal("expected a new identity after revocation")
	}
	if svc.IdentityRefused(context.Background(), node.ID) {
		t.Fatal("a freshly issued identity must not be refused")
	}
}

// TestNodeCertServiceRotateIssuesNewIdentity proves rotation is one atomic step:
// the old identity stops being pinned and the new one takes over immediately.
func TestNodeCertServiceRotateIssuesNewIdentity(t *testing.T) {
	repo := newFakeNodeCertRepo()
	svc := NewNodeCertService(repo, writeCA(t), nil)
	node := &domain.Node{ID: uuid.New(), Name: "n4", Address: "10.0.0.8:50051"}

	first, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rotated, err := svc.Rotate(context.Background(), node)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.Fingerprint == first.Fingerprint {
		t.Fatal("rotation must issue a different identity")
	}
	fp, ok := svc.Fingerprint(context.Background(), node.ID)
	if !ok || fp != rotated.Fingerprint {
		t.Fatalf("pinned fingerprint = %q,%v want the rotated one", fp, ok)
	}
	if rotated.RevokedAt != nil {
		t.Fatal("the replacement certificate must be usable")
	}
}

// TestNodeCertServiceWithoutCAKeyFails makes the failure mode explicit: per-node
// issuance needs the CA key beside ca.crt.
func TestNodeCertServiceWithoutCAKeyFails(t *testing.T) {
	chain, err := pki.Generate(nil)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, chain.CA.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewNodeCertService(newFakeNodeCertRepo(), caPath, nil)
	node := &domain.Node{ID: uuid.New(), Name: "n3", Address: "10.0.0.7:50051"}
	if _, err := svc.Ensure(context.Background(), node); err == nil {
		t.Fatal("expected an error when ca.key is absent")
	}
}

// TestRotatePushDeliversAndPromotes proves the automated path: the new identity
// is handed to the agent, and only after the agent confirms does it become the
// pinned one.
func TestRotatePushDeliversAndPromotes(t *testing.T) {
	repo := newFakeNodeCertRepo()
	svc := NewNodeCertService(repo, writeCA(t), nil)
	node := &domain.Node{ID: uuid.New(), Name: "n5", Address: "10.0.0.9:50051"}
	first, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	var gotCert, gotKey []byte
	var gotFP string
	svc.SetInstaller(func(_ context.Context, _ uuid.UUID, certPEM, keyPEM []byte, fp string) error {
		gotCert, gotKey, gotFP = certPEM, keyPEM, fp
		return nil
	})

	next, pushed, err := svc.RotatePush(context.Background(), node)
	if err != nil {
		t.Fatalf("RotatePush: %v", err)
	}
	if !pushed {
		t.Fatal("expected the certificate to be pushed")
	}
	if next.Fingerprint == first.Fingerprint {
		t.Fatal("rotation must issue a new identity")
	}
	if gotFP != next.Fingerprint || len(gotCert) == 0 || len(gotKey) == 0 {
		t.Fatal("installer did not receive the new identity")
	}
	active, pending := svc.FPair(context.Background(), node.ID)
	if active != next.Fingerprint {
		t.Fatalf("active fingerprint = %q, want %q", active, next.Fingerprint)
	}
	if pending != "" {
		t.Fatalf("pending should be cleared after promotion, got %q", pending)
	}
}

// TestRotatePushFailureLeavesIdentityIntact proves a failed push is a no-op: the
// node keeps the identity it is actually running with, and nothing is left half
// promoted.
func TestRotatePushFailureLeavesIdentityIntact(t *testing.T) {
	repo := newFakeNodeCertRepo()
	svc := NewNodeCertService(repo, writeCA(t), nil)
	node := &domain.Node{ID: uuid.New(), Name: "n6", Address: "10.0.0.10:50051"}
	first, err := svc.Ensure(context.Background(), node)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	svc.SetInstaller(func(context.Context, uuid.UUID, []byte, []byte, string) error {
		return errPushFailed
	})
	_, pushed, err := svc.RotatePush(context.Background(), node)
	if err == nil {
		t.Fatal("expected the push error to surface")
	}
	if pushed {
		t.Fatal("pushed must be false when delivery failed")
	}
	active, pending := svc.FPair(context.Background(), node.ID)
	if active != first.Fingerprint {
		t.Fatalf("active fingerprint changed to %q on a failed push", active)
	}
	if pending != "" {
		t.Fatalf("pending identity must be discarded, got %q", pending)
	}
}
