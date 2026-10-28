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

// EnrollmentLinkRepo stores single-use node enrollment links.
type EnrollmentLinkRepo struct{ pool *pgxpool.Pool }

var _ port.EnrollmentLinkRepository = (*EnrollmentLinkRepo)(nil)

// Create inserts a link. The token itself is not stored, only its hash.
func (r *EnrollmentLinkRepo) Create(ctx context.Context, l *domain.EnrollmentLink) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO enrollment_links (id, node_id, token_hash, expires_at, created_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		l.ID, l.NodeID, l.TokenHash, l.ExpiresAt, l.CreatedAt)
	return err
}

const enrollmentCols = `id, node_id, token_hash, expires_at, used_at, created_at`

func scanEnrollment(row pgx.Row) (*domain.EnrollmentLink, error) {
	var l domain.EnrollmentLink
	if err := row.Scan(&l.ID, &l.NodeID, &l.TokenHash, &l.ExpiresAt, &l.UsedAt, &l.CreatedAt); err != nil {
		return nil, err
	}
	return &l, nil
}

// Redeem consumes a link atomically.
//
// The single UPDATE is the whole guard: it only matches an unused, unexpired
// row, so two concurrent requests cannot both win, and an expired link can never
// be resurrected by a clock skew on the caller side.
func (r *EnrollmentLinkRepo) Redeem(ctx context.Context, tokenHash string) (*domain.EnrollmentLink, error) {
	row := r.pool.QueryRow(ctx,
		`UPDATE enrollment_links
		    SET used_at = now()
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		 RETURNING `+enrollmentCols, tokenHash)
	l, err := scanEnrollment(row)
	if err == pgx.ErrNoRows {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// ListByNode returns links for one node, newest first.
func (r *EnrollmentLinkRepo) ListByNode(ctx context.Context, nodeID uuid.UUID) ([]*domain.EnrollmentLink, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+enrollmentCols+` FROM enrollment_links WHERE node_id = $1 ORDER BY created_at DESC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.EnrollmentLink
	for rows.Next() {
		l, err := scanEnrollment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RevokeByNode expires every unused link for a node.
func (r *EnrollmentLinkRepo) RevokeByNode(ctx context.Context, nodeID uuid.UUID) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE enrollment_links SET expires_at = now()
		  WHERE node_id = $1 AND used_at IS NULL AND expires_at > now()`, nodeID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpired drops links that can no longer be redeemed.
func (r *EnrollmentLinkRepo) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM enrollment_links WHERE expires_at < $1 OR used_at IS NOT NULL`, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
