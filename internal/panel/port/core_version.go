package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// CoreVersionRepository persists the core binary catalog.
type CoreVersionRepository interface {
	// Upsert inserts or updates one (core, version, arch, source) entry.
	Upsert(ctx context.Context, v *domain.CoreVersion) error
	// Get returns one entry by id.
	Get(ctx context.Context, id uuid.UUID) (*domain.CoreVersion, error)
	// List returns every entry, newest version first per core.
	List(ctx context.Context) ([]*domain.CoreVersion, error)
	// ListByCore returns the entries for one core.
	ListByCore(ctx context.Context, core string) ([]*domain.CoreVersion, error)
	// SetArtifact records a finished build: the file, its size and checksum, and
	// promotes the row to ready+metered. Kept separate from Upsert so a periodic
	// release check can never clobber a completed build.
	SetArtifact(ctx context.Context, id uuid.UUID, file string, size int64, sha, url string) error
	// UpdateStatus records a build/serving state change.
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.CoreBuildStatus, errMsg string) error
}
