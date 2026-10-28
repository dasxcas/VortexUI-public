package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vortexui/vortexui/internal/domain"
)

// fakeCore writes a stand-in core binary that answers "version" the way
// sing-box does, without pulling a 36 MB artifact into a unit test.
func fakeCore(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "sing-box-metered")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const meteredVersionOutput = `sing-box version v1.14.2

Environment: go1.26.1 linux/amd64
Tags: with_v2ray_api
Revision: abc123
`

// TestSeedBundledRegistersMeteredBaseline proves the build shipped inside the
// image lands in the catalog as a ready, metered, bundled artifact — the one an
// operator can always fall back to.
func TestSeedBundledRegistersMeteredBaseline(t *testing.T) {
	dir := t.TempDir()
	bin := fakeCore(t, dir, "#!/bin/sh\necho '"+strings.ReplaceAll(meteredVersionOutput, "'", "")+"'\n")

	repo := newFakeCoreRepo()
	dataDir := filepath.Join(dir, "data")
	cat := NewCoreCatalog(repo, nil, dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := cat.SeedBundled(context.Background(), bin); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, _ := repo.ListByCore(context.Background(), "singbox")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Version != "1.14.2" {
		t.Fatalf("version = %q, want 1.14.2", got.Version)
	}
	if !got.Metered || got.Source != domain.CoreBundled || got.Status != domain.CoreReady {
		t.Fatalf("row = %+v", got)
	}
	if got.Arch != runtime.GOARCH {
		t.Fatalf("arch = %q", got.Arch)
	}
	// The artifact must be staged where the download handler serves it from.
	staged := filepath.Join(dataDir, "cores", got.File)
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("artifact not staged: %v", err)
	}
	if got.URL != "/downloads/cores/"+got.File {
		t.Fatalf("url = %q", got.URL)
	}
}

// TestSeedBundledRefusesUnmeteredBinary proves we do not label a build "metered"
// just because it is bundled: a binary without the tag would zero every counter.
func TestSeedBundledRefusesUnmeteredBinary(t *testing.T) {
	dir := t.TempDir()
	bin := fakeCore(t, dir, "#!/bin/sh\necho 'sing-box version v1.14.2'\necho 'Tags: with_quic'\n")

	repo := newFakeCoreRepo()
	cat := NewCoreCatalog(repo, nil, filepath.Join(dir, "data"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := cat.SeedBundled(context.Background(), bin)
	if err == nil {
		t.Fatal("expected a refusal for a build without with_v2ray_api")
	}
	if rows, _ := repo.ListByCore(context.Background(), "singbox"); len(rows) != 0 {
		t.Fatalf("refused artifact was still registered: %+v", rows)
	}
}

// TestSeedBundledMissingPathIsNotAnError covers the lean panel image, which has
// no bundled core at all.
func TestSeedBundledMissingPathIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cat := NewCoreCatalog(newFakeCoreRepo(), nil, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := cat.SeedBundled(context.Background(), ""); err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if err := cat.SeedBundled(context.Background(), filepath.Join(dir, "nope")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
}

// TestSeedBundledOutranksBuildQueue proves a bundled version suppresses the
// automatic build for that same version — the artifact already exists.
func TestSeedBundledOutranksBuildQueue(t *testing.T) {
	repo := newFakeCoreRepo()
	if err := repo.Upsert(context.Background(), &domain.CoreVersion{
		Core: "singbox", Version: "1.14.2", Arch: "amd64",
		Source: domain.CoreFromUpstream, Status: domain.CoreReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(context.Background(), &domain.CoreVersion{
		Core: "singbox", Version: "1.14.2", Arch: "amd64",
		Source: domain.CoreBundled, Status: domain.CoreReady, Metered: true,
	}); err != nil {
		t.Fatal(err)
	}
	builder := &fakeBuilder{}
	cat := NewCoreCatalog(repo, builder, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	cat.queueMissingBuilds(context.Background())
	// The bundled row satisfies amd64, so no amd64 build may be queued. Other
	// architectures are expected to queue: the image only carries the arch it was
	// built for.
	rows, _ := repo.ListByCore(context.Background(), "singbox")
	for _, r := range rows {
		if r.Arch == "amd64" && r.Source == domain.CoreFromBuild {
			t.Fatalf("a build was queued for a version that ships bundled: %+v", r)
		}
	}
}
