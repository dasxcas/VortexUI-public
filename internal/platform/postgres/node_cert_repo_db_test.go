package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vortexui/vortexui/internal/domain"
)

// TestNodeCertRepoRotationAgainstPostgres exercises the pending/promote path the
// automated rotation depends on. Skipped unless a database is provided, so the
// normal `go test ./...` stays hermetic:
//
//	VORTEX_TEST_DATABASE_URL=postgres://... go test ./internal/platform/postgres -run Rotation
func TestNodeCertRepoRotationAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("VORTEX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VORTEX_TEST_DATABASE_URL not set")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	nodeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (id, name, address) VALUES ($1, $2, $3)`,
		nodeID, "cert-test-"+nodeID.String()[:8], "10.9.9.9:50051"); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, nodeID) }()

	repo := &NodeCertRepo{pool: pool}
	active := &domain.NodeCertificate{
		NodeID:      nodeID,
		Serial:      "1",
		Fingerprint: "AAAA1111",
		CertPEM:     []byte("cert-old"),
		KeyPEM:      []byte("key-old"),
		IssuedAt:    time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	}
	if err := repo.Upsert(ctx, active); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pending := *active
	pending.Fingerprint = "BBBB2222"
	pending.CertPEM = []byte("cert-new")
	pending.KeyPEM = []byte("key-new")
	if err := repo.SetPending(ctx, nodeID, &pending); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	// Both fingerprints are acceptable while the push is in flight.
	gotActive, gotPending, err := repo.Fingerprints(ctx, nodeID)
	if err != nil {
		t.Fatalf("fingerprints: %v", err)
	}
	if gotActive != "AAAA1111" || gotPending != "BBBB2222" {
		t.Fatalf("fingerprints = %q,%q", gotActive, gotPending)
	}

	ok, err := repo.PromotePending(ctx, nodeID)
	if err != nil || !ok {
		t.Fatalf("promote: ok=%v err=%v", ok, err)
	}
	gotActive, gotPending, err = repo.Fingerprints(ctx, nodeID)
	if err != nil {
		t.Fatalf("fingerprints after promote: %v", err)
	}
	if gotActive != "BBBB2222" || gotPending != "" {
		t.Fatalf("after promote fingerprints = %q,%q", gotActive, gotPending)
	}

	// The stored key must be the one the node is actually serving.
	stored, err := repo.Get(ctx, nodeID)
	if err != nil || stored == nil {
		t.Fatalf("get: %v", err)
	}
	if string(stored.KeyPEM) != "key-new" || string(stored.CertPEM) != "cert-new" {
		t.Fatalf("stored material = %q/%q", stored.CertPEM, stored.KeyPEM)
	}
	if stored.RevokedAt != nil {
		t.Fatal("promoted certificate must be live")
	}

	// A failed push rolls back to the pending-free state without touching active.
	third := *active
	third.Fingerprint = "CCCC3333"
	if err := repo.SetPending(ctx, nodeID, &third); err != nil {
		t.Fatalf("set pending again: %v", err)
	}
	if err := repo.ClearPending(ctx, nodeID); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	gotActive, gotPending, _ = repo.Fingerprints(ctx, nodeID)
	if gotActive != "BBBB2222" || gotPending != "" {
		t.Fatalf("after failed push fingerprints = %q,%q", gotActive, gotPending)
	}
}
