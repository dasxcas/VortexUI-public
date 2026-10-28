package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// RelayProvisioner turns a relay chain whose first hop is a node this panel
// manages into actual configuration on that node.
//
// Such a hop is transparent: the entry node listens on the hop's port and
// forwards raw TCP to the next hop, so the client's VLESS/Trojan handshake and
// its TLS still terminate at the origin node. That means no credentials are
// duplicated, no users are rebound, and per-user accounting stays where it was.
type RelayProvisioner struct {
	inbounds inboundProvisioner
	nodes    nodeLookup
}

// inboundProvisioner is the narrow slice of InboundService the provisioner
// needs, kept as an interface so it can be tested without a database.
type inboundProvisioner interface {
	ListByNode(ctx context.Context, nodeID uuid.UUID) ([]*domain.Inbound, error)
	Create(ctx context.Context, in CreateInboundInput) (*domain.Inbound, error)
	Update(ctx context.Context, id uuid.UUID, in UpdateInboundInput) (*domain.Inbound, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// nodeLookup resolves the origin node a chain ends on.
type nodeLookup interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Node, error)
}

// NewRelayProvisioner wires the provisioner.
func NewRelayProvisioner(inbounds inboundProvisioner, nodes nodeLookup) *RelayProvisioner {
	return &RelayProvisioner{inbounds: inbounds, nodes: nodes}
}

// relayInboundTag is deterministic so a chain can be re-applied, repaired or
// torn down without storing extra bookkeeping.
func relayInboundTag(chainID uuid.UUID) string {
	return "relay-" + chainID.String()[:8]
}

// managedHop returns the first hop when it is backed by a node we control.
func managedHop(chain *domain.RelayChain) (domain.RelayHop, bool) {
	if chain == nil || len(chain.Hops) == 0 {
		return domain.RelayHop{}, false
	}
	hop := chain.Hops[0]
	if hop.NodeID == nil || hop.Port <= 0 {
		return domain.RelayHop{}, false
	}
	return hop, true
}

// Apply makes the node match the chain: it creates, updates or removes the
// relay inbound. Disabling the chain, or pointing the first hop at something we
// do not manage, removes it — a relay that no longer routes must not keep a
// listening port open.
func (p *RelayProvisioner) Apply(ctx context.Context, chain *domain.RelayChain) error {
	if p == nil || p.inbounds == nil || chain == nil {
		return nil
	}
	hop, managed := managedHop(chain)
	if !chain.Enabled || !managed {
		return p.Remove(ctx, chain)
	}
	host, port, err := p.target(ctx, chain)
	if err != nil {
		return err
	}
	raw := map[string]any{
		"dokodemo": map[string]any{
			"address": host,
			"port":    port,
			"network": "tcp",
		},
	}
	notes := fmt.Sprintf("Managed by relay chain %q — forwards to %s:%d", chain.Name, host, port)

	existing, err := p.find(ctx, *hop.NodeID, relayInboundTag(chain.ID))
	if err != nil {
		return err
	}
	if existing != nil {
		_, err = p.inbounds.Update(ctx, existing.ID, UpdateInboundInput{
			Listen:   listenOrDefault(existing.Listen),
			Port:     hop.Port,
			Network:  "tcp",
			Security: domain.SecurityNone,
			Raw:      &raw,
			Enabled:  true,
			Notes:    notes,
		})
		return err
	}
	_, err = p.inbounds.Create(ctx, CreateInboundInput{
		NodeID:   *hop.NodeID,
		Tag:      relayInboundTag(chain.ID),
		Core:     domain.CoreXray,
		Protocol: domain.ProtoDokodemo,
		Listen:   "0.0.0.0",
		Port:     hop.Port,
		Network:  "tcp",
		Security: domain.SecurityNone,
		Raw:      raw,
		Enabled:  true,
		Notes:    notes,
	})
	return err
}

// Remove deletes the relay inbound for a chain. Safe to call for chains that
// never had one, so it doubles as the cleanup path for disable and delete.
func (p *RelayProvisioner) Remove(ctx context.Context, chain *domain.RelayChain) error {
	if p == nil || p.inbounds == nil || chain == nil {
		return nil
	}
	hop, ok := managedHop(chain)
	if !ok {
		return nil
	}
	existing, err := p.find(ctx, *hop.NodeID, relayInboundTag(chain.ID))
	if err != nil || existing == nil {
		return err
	}
	if err := p.inbounds.Delete(ctx, existing.ID); err != nil {
		// Deleting an inbound also resyncs its node, and an unreachable node
		// makes that fail after the row is already gone. Tear-down must not be
		// blocked by it, so check what actually happened.
		if remaining, lerr := p.find(ctx, *hop.NodeID, relayInboundTag(chain.ID)); lerr == nil && remaining == nil {
			return nil
		}
		return err
	}
	return nil
}

func (p *RelayProvisioner) find(ctx context.Context, nodeID uuid.UUID, tag string) (*domain.Inbound, error) {
	list, err := p.inbounds.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, in := range list {
		if in.Tag == tag {
			return in, nil
		}
	}
	return nil, nil
}

// target resolves where the transparent relay should forward to: the next hop
// when the chain names one, otherwise the origin node's first enabled inbound.
func (p *RelayProvisioner) target(ctx context.Context, chain *domain.RelayChain) (string, int, error) {
	return resolveChainTarget(ctx, p.nodes, p.inbounds, chain)
}

// inboundLister is the read half of the inbound service, shared by provisioning
// and health checks.
type inboundLister interface {
	ListByNode(ctx context.Context, nodeID uuid.UUID) ([]*domain.Inbound, error)
}

// resolveChainTarget answers "where does traffic go after the first hop?" for
// both the provisioner and the health checker, so they can never disagree.
func resolveChainTarget(ctx context.Context, nodes nodeLookup, inbounds inboundLister, chain *domain.RelayChain) (string, int, error) {
	if len(chain.Hops) > 1 {
		next := chain.Hops[1]
		if strings.TrimSpace(next.Address) != "" && next.Port > 0 {
			return next.Address, next.Port, nil
		}
	}
	if nodes == nil {
		return "", 0, fmt.Errorf("relay chain %q: no forward target", chain.Name)
	}
	origin, err := nodes.GetByID(ctx, chain.NodeID)
	if err != nil {
		return "", 0, fmt.Errorf("relay chain %q: origin node: %w", chain.Name, err)
	}
	host := origin.Endpoint
	if host == "" {
		host = hostOf(origin.Address)
	}
	if host == "" {
		return "", 0, fmt.Errorf("relay chain %q: origin node has no reachable address", chain.Name)
	}
	list, err := inbounds.ListByNode(ctx, origin.ID)
	if err != nil {
		return "", 0, err
	}
	for _, in := range list {
		if in.Enabled && in.Port > 0 {
			return host, in.Port, nil
		}
	}
	return "", 0, fmt.Errorf("relay chain %q: origin node %q has no enabled inbound to forward to", chain.Name, origin.Name)
}

func listenOrDefault(v string) string {
	if strings.TrimSpace(v) == "" {
		return "0.0.0.0"
	}
	return v
}
