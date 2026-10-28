package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
)

// RelayService manages CDN/relay chain definitions.
type RelayService struct {
	repo  port.RelayChainRepository
	nodes port.NodeRepository
	prov  *RelayProvisioner
	now   func() time.Time
}

// SetProvisioner wires the component that materialises a chain on the nodes it
// names. Optional: without it chains only affect subscriptions.
func (s *RelayService) SetProvisioner(p *RelayProvisioner) { s.prov = p }

// NewRelayService wires dependencies.
func NewRelayService(repo port.RelayChainRepository, nodes port.NodeRepository) *RelayService {
	return &RelayService{repo: repo, nodes: nodes, now: time.Now}
}

// CreateChainInput describes a new relay chain.
type CreateChainInput struct {
	Name   string
	NodeID uuid.UUID
	Hops   []domain.RelayHop
}

// CreateChain persists a new relay chain. A provisioning failure (port taken,
// node unreachable) comes back as a warning, not an error: the definition is
// saved and can be re-applied once the cause is fixed.
func (s *RelayService) CreateChain(ctx context.Context, in CreateChainInput) (*domain.RelayChain, string, error) {
	if in.Name == "" {
		return nil, "", errors.New("chain name is required")
	}
	if len(in.Hops) == 0 {
		return nil, "", errors.New("at least one hop is required")
	}
	// Verify node exists.
	if _, err := s.nodes.GetByID(ctx, in.NodeID); err != nil {
		return nil, "", errors.New("node not found")
	}
	r := &domain.RelayChain{
		ID:        uuid.New(),
		Name:      in.Name,
		NodeID:    in.NodeID,
		Hops:      in.Hops,
		Enabled:   true,
		CreatedAt: s.now(),
	}
	if err := s.repo.Create(ctx, r); err != nil {
		return nil, "", err
	}
	return r, s.apply(ctx, r), nil
}

// ListChains returns all relay chains.
func (s *RelayService) ListChains(ctx context.Context) ([]*domain.RelayChain, error) {
	return s.repo.List(ctx)
}

// UpdateChain updates a relay chain and reconciles whatever it provisions.
func (s *RelayService) UpdateChain(ctx context.Context, id uuid.UUID, name string, hops []domain.RelayHop, enabled bool) (*domain.RelayChain, string, error) {
	r, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	before := *r
	if name != "" {
		r.Name = name
	}
	if hops != nil {
		r.Hops = hops
	}
	r.Enabled = enabled
	if err := s.repo.Update(ctx, r); err != nil {
		return nil, "", err
	}
	// Moving a managed hop to another node must close the port it used to listen
	// on before the new node starts serving it.
	if oldHop, ok := managedHop(&before); ok && s.prov != nil {
		newHop, stillManaged := managedHop(r)
		if !stillManaged || *newHop.NodeID != *oldHop.NodeID {
			_ = s.prov.Remove(ctx, &before)
		}
	}
	return r, s.apply(ctx, r), nil
}

// ApplyChain re-runs provisioning for a stored chain, so an operator can retry
// after fixing whatever made the first attempt fail.
func (s *RelayService) ApplyChain(ctx context.Context, id uuid.UUID) (*domain.RelayChain, string, error) {
	r, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return r, s.apply(ctx, r), nil
}

// DeleteChain removes a relay chain and the configuration it provisioned.
func (s *RelayService) DeleteChain(ctx context.Context, id uuid.UUID) error {
	r, err := s.repo.GetByID(ctx, id)
	if err == nil && s.prov != nil {
		if rerr := s.prov.Remove(ctx, r); rerr != nil {
			return fmt.Errorf("remove relay inbound: %w", rerr)
		}
	}
	return s.repo.Delete(ctx, id)
}

func (s *RelayService) apply(ctx context.Context, r *domain.RelayChain) string {
	if s.prov == nil {
		return ""
	}
	if err := s.prov.Apply(ctx, r); err != nil {
		return err.Error()
	}
	return ""
}
