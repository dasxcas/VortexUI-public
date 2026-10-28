package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// fakeProber records what it was asked and returns a canned verdict.
type fakeProber struct {
	ok      bool
	latency time.Duration
	msg     string
	err     error

	calls  int
	nodeID uuid.UUID
	target string
}

func (f *fakeProber) Probe(_ context.Context, nodeID uuid.UUID, target string, _ time.Duration) (bool, time.Duration, string, error) {
	f.calls++
	f.nodeID = nodeID
	f.target = target
	return f.ok, f.latency, f.msg, f.err
}

// fakeChainRepo serves chains from a slice.
type fakeChainRepo struct{ chains []*domain.RelayChain }

func (f *fakeChainRepo) Create(context.Context, *domain.RelayChain) error    { return nil }
func (f *fakeChainRepo) Update(context.Context, *domain.RelayChain) error    { return nil }
func (f *fakeChainRepo) Delete(context.Context, uuid.UUID) error             { return nil }
func (f *fakeChainRepo) List(context.Context) ([]*domain.RelayChain, error)   { return f.chains, nil }
func (f *fakeChainRepo) ListByNode(context.Context, uuid.UUID) ([]*domain.RelayChain, error) {
	return f.chains, nil
}

func (f *fakeChainRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.RelayChain, error) {
	for _, c := range f.chains {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, domain.ErrNotFound
}

func newHealthFixture(t *testing.T) (*ChainHealthService, *fakeProber, *domain.RelayChain, uuid.UUID, uuid.UUID) {
	t.Helper()
	entryID, originID := uuid.New(), uuid.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &fakeInboundStore{inbounds: []*domain.Inbound{
		{ID: uuid.New(), NodeID: originID, Tag: "in", Port: 8443, Enabled: true},
	}}
	nodes := fakeNodeLookup{originID: {ID: originID, Name: "origin", Address: "203.0.113.9:50051"}}
	chain := &domain.RelayChain{
		ID: uuid.New(), Name: "vps-hop", NodeID: originID, Enabled: true,
		Hops: []domain.RelayHop{
			{Type: "relay", Address: "198.51.100.7", Port: 443, NodeID: nodeIDPtr(entryID)},
		},
	}
	prober := &fakeProber{ok: true, latency: 12 * time.Millisecond}
	svc := NewChainHealthService(&fakeChainRepo{chains: []*domain.RelayChain{chain}}, nodes, store, prober, log)
	return svc, prober, chain, entryID, originID
}

// TestChainHealthProbesFromTheEntryNode proves the check asks the relay machine
// to dial the forward target — the only vantage point that matters.
func TestChainHealthProbesFromTheEntryNode(t *testing.T) {
	svc, prober, chain, entryID, _ := newHealthFixture(t)

	got, ok := svc.Check(context.Background(), chain)
	if !ok {
		t.Fatal("a managed, enabled chain must be checked")
	}
	if !got.OK || got.LatencyMS != 12 {
		t.Fatalf("health = %+v, want ok with 12ms", got)
	}
	if got.Target != "203.0.113.9:8443" {
		t.Fatalf("target = %q, want the origin's inbound", got.Target)
	}
	if prober.nodeID != entryID {
		t.Fatal("probe must run on the entry node, not the origin")
	}
	if cached, ok := svc.Health(chain.ID); !ok || cached.OK != got.OK {
		t.Fatal("result must be cached for the list endpoint")
	}
}

// TestChainHealthReportsFailure keeps a broken path visible instead of silent.
func TestChainHealthReportsFailure(t *testing.T) {
	svc, prober, chain, _, _ := newHealthFixture(t)
	prober.ok = false
	prober.msg = "dial tcp 203.0.113.9:8443: connect: connection refused"

	got, _ := svc.Check(context.Background(), chain)
	if got.OK {
		t.Fatal("must not report healthy")
	}
	if got.Error == "" {
		t.Fatal("a failure must carry a reason")
	}
}

// TestChainHealthReportsUnreachableNode distinguishes "the relay is down" from
// "the relay cannot reach the origin" — they need different fixes.
func TestChainHealthReportsUnreachableNode(t *testing.T) {
	svc, prober, chain, _, _ := newHealthFixture(t)
	prober.err = errors.New("rpc unavailable")

	got, _ := svc.Check(context.Background(), chain)
	if got.OK || got.Error == "" {
		t.Fatalf("health = %+v", got)
	}
	if want := "entry node unreachable"; len(got.Error) < len(want) || got.Error[:len(want)] != want {
		t.Fatalf("error = %q, want it to name the entry node", got.Error)
	}
}

// TestChainHealthSkipsUnmanagedAndDisabled proves we do not invent a verdict for
// chains the panel cannot probe.
func TestChainHealthSkipsUnmanagedAndDisabled(t *testing.T) {
	svc, prober, chain, _, _ := newHealthFixture(t)

	external := *chain
	external.ID = uuid.New()
	external.Hops = []domain.RelayHop{{Type: "cdn", Address: "front.example.com", Port: 443}}
	if _, ok := svc.Check(context.Background(), &external); ok {
		t.Fatal("an external hop has no path for the panel to check")
	}

	off := *chain
	off.ID = uuid.New()
	off.Enabled = false
	if _, ok := svc.Check(context.Background(), &off); ok {
		t.Fatal("a disabled chain must not be probed")
	}
	if prober.calls != 0 {
		t.Fatalf("prober ran %d times, want 0", prober.calls)
	}
}

// TestChainHealthReportsMissingTarget keeps a chain with nowhere to forward to
// from looking healthy by accident.
func TestChainHealthReportsMissingTarget(t *testing.T) {
	svc, _, chain, _, originID := newHealthFixture(t)
	empty := &fakeInboundStore{}
	svc.inbounds = empty

	got, ok := svc.Check(context.Background(), chain)
	if !ok {
		t.Fatal("chain is still applicable")
	}
	if got.OK || got.Error == "" {
		t.Fatalf("health = %+v, want a target error", got)
	}
	_ = originID
}
