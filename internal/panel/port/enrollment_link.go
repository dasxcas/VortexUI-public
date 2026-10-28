package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// EnrollmentLinkRepository stores single-use join links.
type EnrollmentLinkRepository interface {
	Create(ctx context.Context, link *domain.EnrollmentLink) error
	// Redeem marks the link used and returns it. It must be atomic: two
	// concurrent requests for the same token may not both succeed.
	Redeem(ctx context.Context, tokenHash string) (*domain.EnrollmentLink, error)
	ListByNode(ctx context.Context, nodeID uuid.UUID) ([]*domain.EnrollmentLink, error)
	// RevokeByNode expires every unused link for a node and reports how many
	// were still redeemable, so the console can say what it actually cancelled.
	RevokeByNode(ctx context.Context, nodeID uuid.UUID) (int64, error)
	DeleteExpired(ctx context.Context) (int64, error)
}
