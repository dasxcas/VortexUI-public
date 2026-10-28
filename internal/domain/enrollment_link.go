package domain

import (
	"time"

	"github.com/google/uuid"
)

// EnrollmentLink is a single-use credential for installing one node agent.
//
// The token is never stored: the row keeps a SHA-256 of it, so a database dump
// cannot be replayed against the join endpoint. A link belongs to exactly one
// node, expires quickly, and is consumed by the first fetch.
type EnrollmentLink struct {
	ID        uuid.UUID  `json:"id"`
	NodeID    uuid.UUID  `json:"node_id"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Usable reports whether the link can still be redeemed.
func (l *EnrollmentLink) Usable(now time.Time) bool {
	return l != nil && l.UsedAt == nil && now.Before(l.ExpiresAt)
}
