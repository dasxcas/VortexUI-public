package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// NodeCertRepository persists the per-node mTLS identity.
type NodeCertRepository interface {
	// Get returns the stored certificate, or (nil, nil) when the node has none.
	Get(ctx context.Context, nodeID uuid.UUID) (*domain.NodeCertificate, error)
	// Upsert stores or replaces the node's certificate.
	Upsert(ctx context.Context, cert *domain.NodeCertificate) error
	// Revoke marks the certificate unusable; the next Ensure issues a new one.
	Revoke(ctx context.Context, nodeID uuid.UUID) error
	// Fingerprint returns the pinned fingerprint of a non-revoked certificate.
	Fingerprint(ctx context.Context, nodeID uuid.UUID) (string, bool, error)

	// SetPending records the identity being delivered to the node, keeping the
	// active one usable until the agent confirms the swap.
	SetPending(ctx context.Context, nodeID uuid.UUID, cert *domain.NodeCertificate) error
	// PromotePending makes the pending identity active and clears the old one.
	// Returns false when there is nothing pending.
	PromotePending(ctx context.Context, nodeID uuid.UUID) (bool, error)
	// ClearPending drops a pending identity (push failed or was abandoned).
	ClearPending(ctx context.Context, nodeID uuid.UUID) error
	// Fingerprints returns the pinned active and pending fingerprints.
	Fingerprints(ctx context.Context, nodeID uuid.UUID) (active, pending string, err error)
}
