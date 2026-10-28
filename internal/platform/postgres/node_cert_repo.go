package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
)

// NodeCertRepo stores per-node mTLS identities (see migrations/0057, 0058).
type NodeCertRepo struct{ pool *pgxpool.Pool }

var _ port.NodeCertRepository = (*NodeCertRepo)(nil)

// Get returns the node's certificate, or (nil, nil) when none is stored.
func (r *NodeCertRepo) Get(ctx context.Context, nodeID uuid.UUID) (*domain.NodeCertificate, error) {
	var c domain.NodeCertificate
	var pendingFP, pendingCert, pendingKey *string
	err := r.pool.QueryRow(ctx,
		`SELECT node_id, serial, fingerprint, cert_pem, key_pem, issued_at, expires_at, revoked_at,
		        pending_fingerprint, pending_cert_pem, pending_key_pem
		   FROM node_certificates WHERE node_id = $1`, nodeID).
		Scan(&c.NodeID, &c.Serial, &c.Fingerprint, &c.CertPEM, &c.KeyPEM,
			&c.IssuedAt, &c.ExpiresAt, &c.RevokedAt,
			&pendingFP, &pendingCert, &pendingKey)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pendingFP != nil && pendingCert != nil && pendingKey != nil {
		c.Pending = &domain.NodeCertificate{
			NodeID:      c.NodeID,
			Fingerprint: *pendingFP,
			CertPEM:     []byte(*pendingCert),
			KeyPEM:      []byte(*pendingKey),
			ExpiresAt:   c.ExpiresAt,
		}
	}
	return &c, nil
}

// Upsert stores or replaces the node's certificate.
func (r *NodeCertRepo) Upsert(ctx context.Context, cert *domain.NodeCertificate) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO node_certificates
		     (node_id, serial, fingerprint, cert_pem, key_pem, issued_at, expires_at, revoked_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)
		 ON CONFLICT (node_id) DO UPDATE SET
		     serial = EXCLUDED.serial,
		     fingerprint = EXCLUDED.fingerprint,
		     cert_pem = EXCLUDED.cert_pem,
		     key_pem = EXCLUDED.key_pem,
		     issued_at = EXCLUDED.issued_at,
		     expires_at = EXCLUDED.expires_at,
		     revoked_at = NULL`,
		cert.NodeID, cert.Serial, cert.Fingerprint,
		string(cert.CertPEM), string(cert.KeyPEM),
		cert.IssuedAt, cert.ExpiresAt)
	return err
}

// Revoke marks the certificate unusable while keeping it for the audit trail.
func (r *NodeCertRepo) Revoke(ctx context.Context, nodeID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE node_certificates SET revoked_at = $2
		  WHERE node_id = $1 AND revoked_at IS NULL`, nodeID, time.Now().UTC())
	return err
}

// Fingerprint returns the pinned fingerprint of a live certificate.
func (r *NodeCertRepo) Fingerprint(ctx context.Context, nodeID uuid.UUID) (string, bool, error) {
	var fp string
	err := r.pool.QueryRow(ctx,
		`SELECT fingerprint FROM node_certificates
		  WHERE node_id = $1 AND revoked_at IS NULL AND expires_at > now()`, nodeID).Scan(&fp)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return fp, true, nil
}

// SetPending records an identity that has been (or is being) delivered. The
// active certificate stays usable, so a failed push cannot lock the node out.
func (r *NodeCertRepo) SetPending(ctx context.Context, nodeID uuid.UUID, cert *domain.NodeCertificate) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE node_certificates
		    SET pending_fingerprint = $2, pending_cert_pem = $3, pending_key_pem = $4
		  WHERE node_id = $1`,
		nodeID, cert.Fingerprint, string(cert.CertPEM), string(cert.KeyPEM))
	return err
}

// PromotePending moves the pending identity into the active slot.
func (r *NodeCertRepo) PromotePending(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE node_certificates
		    SET fingerprint = pending_fingerprint,
		        cert_pem = pending_cert_pem,
		        key_pem = pending_key_pem,
		        issued_at = now(),
		        revoked_at = NULL,
		        pending_fingerprint = NULL, pending_cert_pem = NULL, pending_key_pem = NULL
		  WHERE node_id = $1 AND pending_fingerprint IS NOT NULL`, nodeID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClearPending drops a pending identity.
func (r *NodeCertRepo) ClearPending(ctx context.Context, nodeID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE node_certificates
		    SET pending_fingerprint = NULL, pending_cert_pem = NULL, pending_key_pem = NULL
		  WHERE node_id = $1`, nodeID)
	return err
}

// Fingerprints returns the active and pending fingerprints for a node.
func (r *NodeCertRepo) Fingerprints(ctx context.Context, nodeID uuid.UUID) (active, pending string, err error) {
	var a, p *string
	err = r.pool.QueryRow(ctx,
		`SELECT fingerprint, pending_fingerprint FROM node_certificates
		  WHERE node_id = $1 AND revoked_at IS NULL AND expires_at > now()`, nodeID).Scan(&a, &p)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if a != nil {
		active = *a
	}
	if p != nil {
		pending = *p
	}
	return active, pending, nil
}
