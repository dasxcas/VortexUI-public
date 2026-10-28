package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
)

// fakeCoreRepo is an in-memory CoreVersionRepository.
type fakeCoreRepo struct {
	rows map[uuid.UUID]*domain.CoreVersion
}

func newFakeCoreRepo() *fakeCoreRepo {
	return &fakeCoreRepo{rows: map[uuid.UUID]*domain.CoreVersion{}}
}

func (f *fakeCoreRepo) Upsert(_ context.Context, v *domain.CoreVersion) error {
	for id, row := range f.rows {
		if row.Core == v.Core && row.Version == v.Version && row.Arch == v.Arch && row.Source == v.Source {
			cp := *v
			cp.ID = id
			f.rows[id] = &cp
			return nil
		}
	}
	cp := *v
	if cp.ID == uuid.Nil {
		cp.ID = uuid.New()
	}
	f.rows[cp.ID] = &cp
	return nil
}

func (f *fakeCoreRepo) Get(_ context.Context, id uuid.UUID) (*domain.CoreVersion, error) {
	if v, ok := f.rows[id]; ok {
		cp := *v
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeCoreRepo) List(context.Context) ([]*domain.CoreVersion, error) {
	var out []*domain.CoreVersion
	for _, v := range f.rows {
		cp := *v
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeCoreRepo) ListByCore(_ context.Context, core string) ([]*domain.CoreVersion, error) {
	var out []*domain.CoreVersion
	for _, v := range f.rows {
		if v.Core == core {
			cp := *v
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeCoreRepo) SetArtifact(_ context.Context, id uuid.UUID, file string, size int64, sum, url string) error {
	if v, ok := f.rows[id]; ok {
		v.File = file
		v.SizeBytes = size
		v.SHA256 = sum
		v.URL = url
		v.Status = domain.CoreReady
		v.Metered = true
		v.Error = ""
	}
	return nil
}

func (f *fakeCoreRepo) UpdateStatus(_ context.Context, id uuid.UUID, status domain.CoreBuildStatus, errMsg string) error {
	if v, ok := f.rows[id]; ok {
		v.Status = status
		v.Error = errMsg
	}
	return nil
}

// fakeBuilder writes a small artifact without touching docker.
type fakeBuilder struct {
	calls int
	fail  bool
	size  int64
	sum   string
	lastV string
}

func (f *fakeBuilder) Available() bool { return true }
func (f *fakeBuilder) Build(_ context.Context, core, version, arch, destDir string) (string, int64, string, error) {
	f.calls++
	f.lastV = version + "/" + arch
	if f.fail {
		return "", 0, "", errors.New("compiler exploded")
	}
	name := "sing-box-" + version + "-metred-linux-" + arch
	data := []byte("fake binary " + version + " " + arch)
	if err := os.WriteFile(filepath.Join(destDir, name), data, 0o755); err != nil {
		return "", 0, "", err
	}
	sum := f.sum
	if sum == "" {
		sum = "abc123"
	}
	return name, int64(len(data)), sum, nil
}

func fakeGitHub(t *testing.T, releases string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, releases)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testCatalog(t *testing.T, releaseJSON string, builder *fakeBuilder) (*CoreCatalog, *fakeCoreRepo) {
	t.Helper()
	repo := newFakeCoreRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A typed-nil *fakeBuilder would satisfy the interface and defeat the
	// "no builder" path, so pass atrue nil interface when there is no builder.
	var b CoreBuilder
	if builder != nil {
		b = builder
	}
	cat := NewCoreCatalog(repo, b, t.TempDir(), log)
	srv := fakeGitHub(t, releaseJSON)
	cat.client = srv.Client()
	cat.sources = []coreSource{
		{
			core: "singbox",
			api:  srv.URL,
			assetFor: func(arch, version string) string {
				if arch == "amd64" {
					return "sing-box-" + version + "-linux-amd64.tar.gz"
				}
				return ""
			},
			metered: false,
		},
	}
	return cat, repo
}

const singboxReleases = `[
 {"tag_name":"v1.14.4","prerelease":false,"draft":false,"assets":[
   {"name":"sing-box-1.14.4-linux-amd64.tar.gz","browser_download_url":"https://example.test/sb-1.14.4.tar.gz"}]},
 {"tag_name":"v1.14.3-rc1","prerelease":true,"draft":false,"assets":[]}
]`

// TestCatalogRefreshRecordsUpstream proves a release check records the official
// artifact as ready but NOT metered — the stock build cannot count users.
func TestCatalogRefreshRecordsUpstream(t *testing.T) {
	cat, repo := testCatalog(t, singboxReleases, nil)

	n, err := cat.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded %d rows, want 1 (prerelease must be skipped)", n)
	}
	rows, _ := repo.List(context.Background())
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	got := rows[0]
	if got.Version != "1.14.4" || got.Source != domain.CoreFromUpstream {
		t.Fatalf("row = %+v", got)
	}
	if got.Metered {
		t.Fatal("the upstream sing-box build must never be marked metered")
	}
	if got.URL != "https://example.test/sb-1.14.4.tar.gz" {
		t.Fatalf("url = %q", got.URL)
	}
}

// TestCatalogRefreshQueuesBuild proves a new upstream version triggers a metered
// build automatically, and that the build marks the row ready.
func TestCatalogRefreshQueuesBuild(t *testing.T) {
	builder := &fakeBuilder{size: 11, sum: "deadbeef"}
	cat, repo := testCatalog(t, singboxReleases, builder)

	if _, err := cat.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// The queued build runs in a goroutine; wait for the row to settle.
	deadline := time.Now().Add(3 * time.Second)
	var built *domain.CoreVersion
	for time.Now().Before(deadline) {
		rows, _ := repo.ListByCore(context.Background(), "singbox")
		for _, r := range rows {
			if r.Source == domain.CoreFromBuild && r.Status == domain.CoreReady {
				built = r
			}
		}
		if built != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if built == nil {
		t.Fatal("no built artifact was recorded")
	}
	if !built.Metered {
		t.Fatal("a successful build must be marked metered")
	}
	if built.SHA256 != "deadbeef" || built.URL == "" {
		t.Fatalf("built row = %+v", built)
	}
	if builder.lastV != "1.14.4/amd64" && builder.lastV != "1.14.4/arm64" {
		t.Fatalf("unexpected build target %q", builder.lastV)
	}
}

// TestCatalogBuildFailureIsRecorded proves a broken build leaves a failed row
// with the reason, and never becomes selectable.
func TestCatalogBuildFailureIsRecorded(t *testing.T) {
	builder := &fakeBuilder{fail: true}
	cat, repo := testCatalog(t, singboxReleases, builder)

	if _, err := cat.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := repo.ListByCore(context.Background(), "singbox")
		for _, r := range rows {
			if r.Source == domain.CoreFromBuild && r.Status == domain.CoreFailed {
				if r.Error == "" {
					t.Fatal("failed row must carry the reason")
				}
				if r.Usable() {
					t.Fatal("a failed build must not be usable")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("failed build was never recorded")
}
