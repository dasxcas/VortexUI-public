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

// CoreVersionRepo stores the core binary catalog (see migrations/0059).
type CoreVersionRepo struct{ pool *pgxpool.Pool }

var _ port.CoreVersionRepository = (*CoreVersionRepo)(nil)

// Upsert inserts a catalog entry or refreshes the metadata of an existing one.
// Build results (metered, sha256, status) are preserved when the incoming row
// has nothing to say about them, so a periodic release check never erases a
// verified build.
func (r *CoreVersionRepo) Upsert(ctx context.Context, v *domain.CoreVersion) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO core_versions
		     (id, core, version, arch, source, metered, status, url, file, sha256, size_bytes, error)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (core, version, arch, source) DO UPDATE SET
		     metered    = CASE WHEN EXCLUDED.metered THEN TRUE ELSE core_versions.metered END,
		     status     = CASE WHEN core_versions.status = 'building' THEN core_versions.status ELSE EXCLUDED.status END,
		     url        = CASE WHEN EXCLUDED.url <> '' THEN EXCLUDED.url ELSE core_versions.url END,
		     file       = CASE WHEN EXCLUDED.file <> '' THEN EXCLUDED.file ELSE core_versions.file END,
		     sha256     = CASE WHEN EXCLUDED.sha256 <> '' THEN EXCLUDED.sha256 ELSE core_versions.sha256 END,
		     size_bytes = CASE WHEN EXCLUDED.size_bytes > 0 THEN EXCLUDED.size_bytes ELSE core_versions.size_bytes END,
		     error      = EXCLUDED.error,
		     updated_at = now()`,
		v.ID, v.Core, v.Version, v.Arch, v.Source, v.Metered, v.Status,
		v.URL, v.File, v.SHA256, v.SizeBytes, v.Error)
	return err
}

const coreVersionCols = `id, core, version, arch, source, metered, status, url, file, sha256, size_bytes, error, created_at, updated_at`

func scanCoreVersion(row pgx.Row) (*domain.CoreVersion, error) {
	var v domain.CoreVersion
	err := row.Scan(&v.ID, &v.Core, &v.Version, &v.Arch, &v.Source, &v.Metered, &v.Status,
		&v.URL, &v.File, &v.SHA256, &v.SizeBytes, &v.Error, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Get returns one catalog entry.
func (r *CoreVersionRepo) Get(ctx context.Context, id uuid.UUID) (*domain.CoreVersion, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+coreVersionCols+` FROM core_versions WHERE id = $1`, id)
	v, err := scanCoreVersion(row)
	if err == pgx.ErrNoRows {
		return nil, domain.ErrNotFound
	}
	return v, err
}

// List returns the whole catalog, newest first within each core.
func (r *CoreVersionRepo) List(ctx context.Context) ([]*domain.CoreVersion, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+coreVersionCols+` FROM core_versions ORDER BY core, created_at DESC, version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.CoreVersion
	for rows.Next() {
		v, err := scanCoreVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListByCore returns the catalog for one core.
func (r *CoreVersionRepo) ListByCore(ctx context.Context, core string) ([]*domain.CoreVersion, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+coreVersionCols+` FROM core_versions WHERE core = $1 ORDER BY created_at DESC, version DESC`, core)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.CoreVersion
	for rows.Next() {
		v, err := scanCoreVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetArtifact records a finished build and promotes the row.
func (r *CoreVersionRepo) SetArtifact(ctx context.Context, id uuid.UUID, file string, size int64, sha, url string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE core_versions
		    SET file = $2, size_bytes = $3, sha256 = $4, url = $5,
		        status = 'ready', metered = TRUE, error = '', updated_at = now()
		  WHERE id = $1`, id, file, size, sha, url)
	return err
}

// UpdateStatus records a state change and stamps updated_at.
func (r *CoreVersionRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.CoreBuildStatus, errMsg string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE core_versions SET status = $2, error = $3, updated_at = $4 WHERE id = $1`,
		id, status, errMsg, time.Now().UTC())
	return err
}
