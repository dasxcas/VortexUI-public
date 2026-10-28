package service

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

type fakeLinkRepo struct {
	rows []*domain.EnrollmentLink
}

func (f *fakeLinkRepo) Create(_ context.Context, l *domain.EnrollmentLink) error {
	cp := *l
	f.rows = append(f.rows, &cp)
	return nil
}

// Redeem mirrors the SQL: an unused, unexpired row is marked used and returned;
// everything else is "not found".
func (f *fakeLinkRepo) Redeem(_ context.Context, hash string) (*domain.EnrollmentLink, error) {
	for _, l := range f.rows {
		if l.TokenHash != hash {
			continue
		}
		if l.UsedAt != nil || !time.Now().Before(l.ExpiresAt) {
			return nil, domain.ErrNotFound
		}
		now := time.Now()
		l.UsedAt = &now
		cp := *l
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeLinkRepo) ListByNode(context.Context, uuid.UUID) ([]*domain.EnrollmentLink, error) {
	return f.rows, nil
}

func (f *fakeLinkRepo) DeleteExpired(context.Context) (int64, error) { return 0, nil }

func (f *fakeLinkRepo) RevokeByNode(_ context.Context, nodeID uuid.UUID) (int64, error) {
	var n int64
	for _, l := range f.rows {
		if l.NodeID == nodeID && l.UsedAt == nil && time.Now().Before(l.ExpiresAt) {
			l.ExpiresAt = time.Now().Add(-time.Second)
			n++
		}
	}
	return n, nil
}

type fakeNodeGetter struct{ node *domain.Node }

func (f *fakeNodeGetter) Get(context.Context, uuid.UUID) (*domain.Node, error) { return f.node, nil }

type fakeCertIssuer struct{}

func (fakeCertIssuer) Ensure(context.Context, *domain.Node) (*domain.NodeCertificate, error) {
	return &domain.NodeCertificate{CertPEM: []byte("cert"), KeyPEM: []byte("key")}, nil
}

type fakeBundler struct{}

func (fakeBundler) BundleFor(*domain.NodeCertificate) (*EnrollmentBundle, error) {
	return &EnrollmentBundle{Bundle: "YnVuZGxl"}, nil
}

func newJoinFixture(t *testing.T) (*NodeJoin, *fakeLinkRepo, *domain.Node) {
	t.Helper()
	node := &domain.Node{ID: uuid.New(), Name: "edge-1", Address: "10.0.0.5:50051"}
	repo := &fakeLinkRepo{}
	j := NewNodeJoin(repo, &fakeNodeGetter{node: node}, fakeCertIssuer{}, fakeBundler{},
		"https://panel.example.com", "/opt/vortexui/agent", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return j, repo, node
}

// TestJoinLinkIsSingleUse is the property that makes a link safe to paste: the
// first fetch consumes it, the second finds nothing.
func TestJoinLinkIsSingleUse(t *testing.T) {
	j, _, node := newJoinFixture(t)
	link, err := j.Create(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(link.Command, "curl -fsSL https://panel.example.com/join/") {
		t.Fatalf("command = %q", link.Command)
	}
	script, err := j.Redeem(context.Background(), link.Token)
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if !strings.Contains(script, `PANEL="https://panel.example.com"`) {
		t.Fatal("script does not carry the panel url")
	}
	if !strings.Contains(script, `BUNDLE="YnVuZGxl"`) {
		t.Fatal("script does not embed the certificate bundle")
	}
	if !strings.Contains(script, `AGENT_URL="https://panel.example.com/downloads/agent/vortex-node"`) {
		t.Fatal("script does not point at the panel-served agent")
	}
	if _, err := j.Redeem(context.Background(), link.Token); err == nil {
		t.Fatal("a second redeem must fail")
	}
}

// TestExpiredJoinLinkIsRejected covers the clock, not just the used flag.
func TestExpiredJoinLinkIsRejected(t *testing.T) {
	j, _, node := newJoinFixture(t)
	j.SetTTL(time.Millisecond)
	link, err := j.Create(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := j.Redeem(context.Background(), link.Token); err == nil {
		t.Fatal("an expired link must not redeem")
	}
}

// TestJoinNeedsPublicURL proves the failure is explicit instead of minting a
// command with a relative URL that no node could fetch.
func TestJoinNeedsPublicURL(t *testing.T) {
	j, _, node := newJoinFixture(t)
	j.publicURL = ""
	if _, err := j.Create(context.Background(), node.ID); err == nil {
		t.Fatal("expected an error when VORTEX_PUBLIC_URL is empty")
	}
}

// TestJoinEscapesNodeName proves a hostile node name cannot break out of the
// generated shell literal.
func TestJoinEscapesNodeName(t *testing.T) {
	j, _, node := newJoinFixture(t)
	node.Name = `edge"; rm -rf / #`
	link, err := j.Create(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	script, err := j.Redeem(context.Background(), link.Token)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if strings.Contains(script, `NODE_NAME="edge"; rm -rf /`) {
		t.Fatal("node name was not escaped")
	}
	if !strings.Contains(script, `NODE_NAME="edge\"; rm -rf / #"`) {
		t.Fatalf("unexpected escaping: %s", firstLineWith(script, "NODE_NAME="))
	}
}

// TestRevokeAllKillsOutstandingLinks proves a leaked command can be cancelled
// before its TTL runs out.
func TestRevokeAllKillsOutstandingLinks(t *testing.T) {
	j, _, node := newJoinFixture(t)
	link, err := j.Create(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := j.RevokeAll(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
	if _, err := j.Redeem(context.Background(), link.Token); err == nil {
		t.Fatal("a revoked link must not redeem")
	}
}

func firstLineWith(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
