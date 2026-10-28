package domain

import (
	"time"

	"github.com/google/uuid"
)

// NodeCertificate is the per-node mTLS identity issued by the panel's CA. Each
// node gets its own keypair, so a stolen node key identifies exactly one node
// and can be revoked without touching the rest of the fleet.
type NodeCertificate struct {
	NodeID      uuid.UUID  `json:"node_id"`
	Serial      string     `json:"serial"`
	Fingerprint string     `json:"fingerprint"` // SHA-256 of DER, uppercase hex
	CertPEM     []byte     `json:"-"`           // never serialized to API clients
	KeyPEM      []byte     `json:"-"`
	IssuedAt    time.Time  `json:"issued_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`

	// Pending is the identity delivered to the node but not yet promoted. It is
	// populated only between the push and the agent's confirmation.
	Pending *NodeCertificate `json:"pending,omitempty"`
}

// Usable reports whether the certificate can still authenticate the node.
func (c *NodeCertificate) Usable(now time.Time, renewBefore time.Duration) bool {
	if c == nil || c.RevokedAt != nil {
		return false
	}
	return c.ExpiresAt.After(now.Add(renewBefore))
}
