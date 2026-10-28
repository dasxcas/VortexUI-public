package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// fakeInboundStore is an in-memory inboundProvisioner.
type fakeInboundStore struct {
	inbounds []*domain.Inbound
	created  int
	updated  int
	deleted  int
}

func (f *fakeInboundStore) ListByNode(_ context.Context, nodeID uuid.UUID) ([]*domain.Inbound, error) {
	var out []*domain.Inbound
	for _, in := range f.inbounds {
		if in.NodeID == nodeID {
			out = append(out, in)
		}
	}
	return out, nil
}

func (f *fakeInboundStore) Create(_ context.Context, in CreateInboundInput) (*domain.Inbound, error) {
	f.created++
	rec := &domain.Inbound{
		ID: uuid.New(), NodeID: in.NodeID, Tag: in.Tag, Protocol: in.Protocol,
		Listen: in.Listen, Port: in.Port, Network: in.Network, Security: in.Security,
		Raw: in.Raw, Enabled: in.Enabled, Notes: in.Notes,
	}
	f.inbounds = append(f.inbounds, rec)
	return rec, nil
}

func (f *fakeInboundStore) Update(_ context.Context, id uuid.UUID, in UpdateInboundInput) (*domain.Inbound, error) {
	f.updated++
	for _, rec := range f.inbounds {
		if rec.ID != id {
			continue
		}
		rec.Port = in.Port
		rec.Listen = in.Listen
		rec.Network = in.Network
		rec.Security = in.Security
		rec.Enabled = in.Enabled
		rec.Notes = in.Notes
		if in.Raw != nil {
			rec.Raw = *in.Raw
		}
		return rec, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeInboundStore) Delete(_ context.Context, id uuid.UUID) error {
	f.deleted++
	for i, rec := range f.inbounds {
		if rec.ID == id {
			f.inbounds = append(f.inbounds[:i], f.inbounds[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

type fakeNodeLookup map[uuid.UUID]*domain.Node

func (f fakeNodeLookup) GetByID(_ context.Context, id uuid.UUID) (*domain.Node, error) {
	if n, ok := f[id]; ok {
		return n, nil
	}
	return nil, domain.ErrNotFound
}

func nodeIDPtr(id uuid.UUID) *uuid.UUID { return &id }

// TestRelayProvisionCreatesTransparentForwarder proves a managed hop becomes a
// dokodemo listener that forwards raw TCP to the origin — no credentials, no
// protocol, nothing the client has to know about.
func TestRelayProvisionCreatesTransparentForwarder(t *testing.T) {
	entryID, originID := uuid.New(), uuid.New()
	store := &fakeInboundStore{}
	nodes := fakeNodeLookup{
		originID: {ID: originID, Name: "origin", Address: "203.0.113.9:50051"},
	}
	prov := NewRelayProvisioner(store, nodes)

	chain := &domain.RelayChain{
		ID: uuid.New(), Name: "vps-front", NodeID: originID, Enabled: true, CreatedAt: time.Now(),
		Hops: []domain.RelayHop{
			{Type: "relay", Address: "198.51.100.7", Port: 443, NodeID: nodeIDPtr(entryID)},
			{Type: "relay", Address: "203.0.113.9", Port: 8443},
		},
	}
	if err := prov.Apply(context.Background(), chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if store.created != 1 || len(store.inbounds) != 1 {
		t.Fatalf("expected one provisioned inbound, got %d", store.created)
	}
	in := store.inbounds[0]
	if in.NodeID != entryID {
		t.Fatal("relay must be created on the entry node")
	}
	if in.Protocol != domain.ProtoDokodemo || in.Port != 443 || in.Network != "tcp" {
		t.Fatalf("unexpected inbound: %+v", in)
	}
	dd, _ := in.Raw["dokodemo"].(map[string]any)
	if dd["address"] != "203.0.113.9" || dd["port"] != 8443 {
		t.Fatalf("forward target = %v:%v, want 203.0.113.9:8443", dd["address"], dd["port"])
	}
	if in.Tag != relayInboundTag(chain.ID) {
		t.Fatalf("tag = %q", in.Tag)
	}
}

// TestRelayProvisionFallsBackToOriginInbound covers the two-hop chain: with no
// explicit second hop the relay forwards to the origin's first enabled inbound.
func TestRelayProvisionFallsBackToOriginInbound(t *testing.T) {
	entryID, originID := uuid.New(), uuid.New()
	store := &fakeInboundStore{inbounds: []*domain.Inbound{
		{ID: uuid.New(), NodeID: originID, Tag: "disabled", Port: 1111, Enabled: false},
		{ID: uuid.New(), NodeID: originID, Tag: "live", Port: 8443, Enabled: true},
	}}
	nodes := fakeNodeLookup{originID: {ID: originID, Name: "origin", Address: "203.0.113.9:50051"}}
	prov := NewRelayProvisioner(store, nodes)

	chain := &domain.RelayChain{
		ID: uuid.New(), Name: "front", NodeID: originID, Enabled: true,
		Hops: []domain.RelayHop{{Type: "relay", Port: 443, NodeID: nodeIDPtr(entryID)}},
	}
	if err := prov.Apply(context.Background(), chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(store.inbounds) != 3 {
		t.Fatalf("expected the new inbound, got %d total", len(store.inbounds))
	}
	created := store.inbounds[len(store.inbounds)-1]
	dd, _ := created.Raw["dokodemo"].(map[string]any)
	if dd["address"] != "203.0.113.9" || dd["port"] != 8443 {
		t.Fatalf("target = %v:%v, want the origin's live inbound", dd["address"], dd["port"])
	}
}

// TestRelayProvisionIsIdempotent proves re-applying updates in place instead of
// stacking listeners, and that disabling removes the port entirely.
func TestRelayProvisionIsIdempotent(t *testing.T) {
	entryID, originID := uuid.New(), uuid.New()
	store := &fakeInboundStore{}
	nodes := fakeNodeLookup{originID: {ID: originID, Name: "origin", Address: "203.0.113.9:50051"}}
	prov := NewRelayProvisioner(store, nodes)

	chain := &domain.RelayChain{
		ID: uuid.New(), Name: "front", NodeID: originID, Enabled: true,
		Hops: []domain.RelayHop{
			{Type: "relay", Port: 443, NodeID: nodeIDPtr(entryID)},
			{Type: "relay", Address: "203.0.113.9", Port: 8443},
		},
	}
	if err := prov.Apply(context.Background(), chain); err != nil {
		t.Fatalf("apply: %v", err)
	}
	chain.Hops[0].Port = 8443
	if err := prov.Apply(context.Background(), chain); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if store.created != 1 || store.updated != 1 {
		t.Fatalf("created=%d updated=%d, want 1/1", store.created, store.updated)
	}
	if store.inbounds[0].Port != 8443 {
		t.Fatalf("port = %d, want the updated 8443", store.inbounds[0].Port)
	}

	chain.Enabled = false
	if err := prov.Apply(context.Background(), chain); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if store.deleted != 1 || len(store.inbounds) != 0 {
		t.Fatalf("disabling must remove the relay port (deleted=%d left=%d)", store.deleted, len(store.inbounds))
	}
}

// TestRelayProvisionReportsUnreachableOrigin makes the failure legible instead
// of silently creating a forwarder pointing nowhere.
func TestRelayProvisionReportsUnreachableOrigin(t *testing.T) {
	entryID, originID := uuid.New(), uuid.New()
	store := &fakeInboundStore{}
	nodes := fakeNodeLookup{originID: {ID: originID, Name: "empty", Address: "203.0.113.9:50051"}}
	prov := NewRelayProvisioner(store, nodes)

	chain := &domain.RelayChain{
		ID: uuid.New(), Name: "front", NodeID: originID, Enabled: true,
		Hops: []domain.RelayHop{{Type: "relay", Port: 443, NodeID: nodeIDPtr(entryID)}},
	}
	err := prov.Apply(context.Background(), chain)
	if err == nil || !strings.Contains(err.Error(), "no enabled inbound") {
		t.Fatalf("want a clear error, got %v", err)
	}
	if store.created != 0 {
		t.Fatal("nothing should be created when the target is unknown")
	}
}
