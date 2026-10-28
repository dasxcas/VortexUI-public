package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
)

// CoreCatalog tracks which core binaries exist, where they came from, and
// whether a build can meter traffic per user.
//
// The vendor's sing-box release cannot: it is compiled without with_v2ray_api.
// So the catalog keeps two kinds of sing-box entry — the upstream artifact
// (metered=false) and a self-built one produced here (metered=true only after
// reading the tag back out of the binary).
type CoreCatalog struct {
	repo    port.CoreVersionRepository
	builder CoreBuilder
	dataDir string
	log     *slog.Logger
	client  *http.Client
	now     func() time.Time

	mu       sync.Mutex
	building map[uuid.UUID]bool
	sources  []coreSource
	// buildSem serialises docker builds: the daemon handles concurrent pulls
	// badly, and each build already saturates a few cores.
	buildSem chan struct{}
}

// CoreBuilder compiles one core version into dataDir and returns the artifact.
type CoreBuilder interface {
	// Available reports whether this host can build (Docker present, opted in).
	Available() bool
	// Build writes the artifact into destDir and returns its file name, size and
	// sha256. It must fail if the produced binary lacks with_v2ray_api.
	Build(ctx context.Context, core, version, arch, destDir string) (string, int64, string, error)
}

// NewCoreCatalog wires the catalog. builder may be nil (builds disabled).
func NewCoreCatalog(repo port.CoreVersionRepository, builder CoreBuilder, dataDir string, log *slog.Logger) *CoreCatalog {
	return &CoreCatalog{
		repo:     repo,
		builder:  builder,
		dataDir:  dataDir,
		log:      log,
		client:   &http.Client{Timeout: 60 * time.Second},
		now:      time.Now,
		building: map[uuid.UUID]bool{},
		sources:  upstreamSources,
		buildSem: make(chan struct{}, 1),
	}
}

// DataDir returns where artifacts are stored, for the download handler.
func (c *CoreCatalog) DataDir() string { return c.dataDir }

// SeedBundled registers the metering build that ships inside the panel image.
//
// This is the one artifact an operator can always fall back to when a newer
// build turns out not to count traffic, so it is seeded on every start, marked
// metered, and never removed. Seeding is a no-op when the image has no bundled
// build (the lean panel target does not).
func (c *CoreCatalog) SeedBundled(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil // image without a bundled build
	}
	version, tags, err := coreVersionOf(ctx, path)
	if err != nil {
		return fmt.Errorf("bundled metered core at %s: %w", path, err)
	}
	if !strings.Contains(tags, "with_v2ray_api") {
		// Shipping a non-metering binary under this name would be a lie: a node
		// would install it expecting counters and get zeros.
		return fmt.Errorf("bundled core %s lacks with_v2ray_api (tags: %s)", path, tags)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.dataDir, "cores")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("sing-box-%s-metred-linux-%s", version, runtime.GOARCH)
	dest := filepath.Join(dir, name)
	if err := linkOrCopy(path, dest); err != nil {
		return fmt.Errorf("stage bundled core: %w", err)
	}
	row := &domain.CoreVersion{
		ID:        uuid.New(),
		Core:      "singbox",
		Version:   version,
		Arch:      runtime.GOARCH,
		Source:    domain.CoreBundled,
		Status:    domain.CoreReady,
		Metered:   true,
		File:      name,
		URL:       "/downloads/cores/" + name,
		SHA256:    sum,
		SizeBytes: info.Size(),
	}
	if err := c.repo.Upsert(ctx, row); err != nil {
		return err
	}
	c.log.Info("bundled metered core registered", "version", version, "arch", runtime.GOARCH,
		"size", info.Size())
	return nil
}

// coreVersionOf reads the version and build tags out of a core binary.
func coreVersionOf(ctx context.Context, path string) (string, string, error) {
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("run version: %w (%s)", err, firstNonEmptyLine(string(out)))
	}
	var version, tags string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Tags:") {
			tags = strings.TrimSpace(strings.TrimPrefix(line, "Tags:"))
		}
		if version == "" {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "version" && i+1 < len(fields) {
					version = strings.TrimPrefix(fields[i+1], "v")
					break
				}
			}
		}
	}
	if version == "" {
		return "", tags, fmt.Errorf("cannot parse a version from %q", firstNonEmptyLine(string(out)))
	}
	return version, tags, nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// linkOrCopy hardlinks when possible so a 36 MB artifact is not duplicated.
func linkOrCopy(src, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	_ = os.Remove(dest)
	if err := os.Link(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Get returns one catalog entry.
func (c *CoreCatalog) Get(ctx context.Context, id uuid.UUID) (*domain.CoreVersion, error) {
	return c.repo.Get(ctx, id)
}

// List returns the catalog.
func (c *CoreCatalog) List(ctx context.Context) ([]*domain.CoreVersion, error) {
	return c.repo.List(ctx)
}

// release mirrors the fields of the GitHub releases API we care about.
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
	Prerelease bool `json:"prerelease"`
	Draft      bool `json:"draft"`
}

// coreSource describes one upstream repository the catalog tracks.
type coreSource struct {
	core string
	api  string
	// assetFor maps an architecture to the release asset name.
	assetFor func(arch, version string) string
	metered  bool
}

// upstreamSources are the repositories the catalog tracks by default.
var upstreamSources = []coreSource{
	{
		core: "xray",
		api:  "https://api.github.com/repos/XTLS/Xray-core/releases",
		assetFor: func(arch, _ string) string {
			switch arch {
			case "amd64":
				return "Xray-linux-64.zip"
			case "arm64":
				return "Xray-linux-arm64-v8a.zip"
			}
			return ""
		},
		// Xray ships its stats API in the standard build.
		metered: true,
	},
	{
		core: "singbox",
		api:  "https://api.github.com/repos/SagerNet/sing-box/releases",
		assetFor: func(arch, version string) string {
			suffix := map[string]string{"amd64": "amd64", "arm64": "arm64"}[arch]
			if suffix == "" {
				return ""
			}
			return fmt.Sprintf("sing-box-%s-linux-%s.tar.gz", version, suffix)
		},
		// The official sing-box build has no with_v2ray_api.
		metered: false,
	},
}

// Refresh pulls the upstream release list and records anything new. New sing-box
// versions are queued for a metered build when a builder is available, so an
// operator never has to remember to compile anything.
func (c *CoreCatalog) Refresh(ctx context.Context) (int, error) {
	added := 0
	for _, src := range c.sources {
		rels, err := c.fetchReleases(ctx, src.api)
		if err != nil {
			c.log.Warn("core catalog: release check failed", "core", src.core, "err", err)
			continue
		}
		for _, rel := range rels {
			if rel.Draft || rel.Prerelease {
				continue
			}
			version := strings.TrimPrefix(rel.TagName, "v")
			for _, arch := range []string{"amd64", "arm64"} {
				want := src.assetFor(arch, version)
				if want == "" {
					continue
				}
				url := assetURL(rel, want)
				if url == "" {
					continue
				}
				err := c.repo.Upsert(ctx, &domain.CoreVersion{
					ID:      uuid.New(),
					Core:    src.core,
					Version: version,
					Arch:    arch,
					Source:  domain.CoreFromUpstream,
					Status:  domain.CoreReady,
					Metered: src.metered,
					URL:     url,
				})
				if err != nil {
					return added, err
				}
				added++
			}
		}
	}
	c.queueMissingBuilds(ctx)
	return added, nil
}

func assetURL(rel release, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

func (c *CoreCatalog) fetchReleases(ctx context.Context, api string) ([]release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"?per_page=15", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: http %d", resp.StatusCode)
	}
	var rels []release
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	return rels, nil
}

// queueMissingBuilds starts a build for the newest sing-box versions that have
// no ready metered artifact yet. Only the latest two are attempted, so a first
// run does not compile the entire release history.
func (c *CoreCatalog) queueMissingBuilds(ctx context.Context) {
	if c.builder == nil || !c.builder.Available() {
		return
	}
	rows, err := c.repo.ListByCore(ctx, "singbox")
	if err != nil {
		c.log.Warn("core catalog: list singbox versions failed", "err", err)
		return
	}
	have := map[string]bool{}
	newest := []string{}
	for _, v := range rows {
		if (v.Source == domain.CoreFromBuild || v.Source == domain.CoreBundled) &&
			v.Status == domain.CoreReady && v.Metered {
			have[v.Version+"|"+v.Arch] = true
		}
	}
	// rows are ordered newest-first per core by the repository.
	for _, v := range rows {
		if v.Source != domain.CoreFromUpstream || v.Arch != "amd64" {
			continue
		}
		if len(newest) >= 2 {
			break
		}
		newest = append(newest, v.Version)
	}
	for _, version := range newest {
		for _, arch := range []string{"amd64", "arm64"} {
			if have[version+"|"+arch] {
				continue
			}
			row := &domain.CoreVersion{
				ID:      uuid.New(),
				Core:    "singbox",
				Version: version,
				Arch:    arch,
				Source:  domain.CoreFromBuild,
				Status:  domain.CoreBuilding,
			}
			if err := c.repo.Upsert(ctx, row); err != nil {
				c.log.Warn("core catalog: queue build failed", "version", version, "arch", arch, "err", err)
				continue
			}
			go c.Build(context.Background(), row.ID)
		}
	}
}

// Build compiles one catalog entry, verifies the produced binary and marks it
// ready. It is safe to call twice: a second call returns immediately.
func (c *CoreCatalog) Build(ctx context.Context, id uuid.UUID) error {
	c.mu.Lock()
	if c.building[id] {
		c.mu.Unlock()
		return errors.New("build already running")
	}
	c.building[id] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.building, id)
		c.mu.Unlock()
	}()

	select {
	case c.buildSem <- struct{}{}:
		defer func() { <-c.buildSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	row, err := c.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if c.builder == nil || !c.builder.Available() {
		_ = c.repo.UpdateStatus(ctx, id, domain.CoreFailed, "core building is disabled (set VORTEX_CORE_BUILD=true and install Docker)")
		return errors.New("core building is disabled")
	}
	_ = c.repo.UpdateStatus(ctx, id, domain.CoreBuilding, "")

	dir := filepath.Join(c.dataDir, "cores")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = c.repo.UpdateStatus(ctx, id, domain.CoreFailed, err.Error())
		return err
	}
	file, size, sum, err := c.builder.Build(ctx, row.Core, row.Version, row.Arch, dir)
	if err != nil {
		c.log.Error("core build failed", "core", row.Core, "version", row.Version, "arch", row.Arch, "err", err)
		_ = c.repo.UpdateStatus(ctx, id, domain.CoreFailed, err.Error())
		return err
	}
	row.File = file
	row.SizeBytes = size
	row.SHA256 = sum
	row.Status = domain.CoreReady
	row.Metered = true // Build only returns on success, and success requires the tag
	row.URL = fmt.Sprintf("/downloads/cores/%s", file)
	row.Error = ""
	if err := c.repo.SetArtifact(ctx, id, file, size, sum, row.URL); err != nil {
		return err
	}
	short := sum
	if len(short) > 12 {
		short = short[:12]
	}
	c.log.Info("core built", "core", row.Core, "version", row.Version, "arch", row.Arch,
		"size", size, "sha256", short)
	return nil
}

// ── Docker builder ───────────────────────────────────────────────────────────

// DockerBuilder compiles cores in a throwaway golang container, so the panel
// host needs no Go toolchain of its own.
type DockerBuilder struct {
	Image   string
	Timeout time.Duration
	// Network is the docker network mode for the build container. Defaults to
	// bridge; hosts whose bridge resolver is broken (no DNS) can set
	// VORTEX_CORE_BUILD_NETWORK=host.
	Network string
	log     *slog.Logger
}

// NewDockerBuilder returns a builder that shells out to docker.
func NewDockerBuilder(log *slog.Logger) *DockerBuilder {
	net := strings.TrimSpace(os.Getenv("VORTEX_CORE_BUILD_NETWORK"))
	if net == "" {
		net = "bridge"
	}
	return &DockerBuilder{Image: "golang:1.25-bookworm", Timeout: 20 * time.Minute, Network: net, log: log}
}

// Available reports whether docker is usable on this host.
func (b *DockerBuilder) Available() bool {
	if b == nil {
		return false
	}
	return exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run() == nil
}

// buildScript compiles one version with the metering tag and refuses to exit
// successfully if the tag is missing from the produced binary.
const buildScript = `set -e
mkdir -p /src && cd /src
[ -f go.mod ] || go mod init vbuild >/dev/null 2>&1
GOFLAGS=-mod=mod go get github.com/sagernet/sing-box@v${VER} >/dev/null 2>&1
GOFLAGS=-mod=mod go get github.com/sagernet/sing-box/cmd/sing-box@v${VER} >/dev/null 2>&1
GOFLAGS=-mod=mod go build -tags with_v2ray_api -trimpath \
  -ldflags "-s -w -X github.com/sagernet/sing-box/constant.Version=v${VER}" \
  -o /out/sing-box github.com/sagernet/sing-box/cmd/sing-box
go version -m /out/sing-box | grep -q 'with_v2ray_api' || { echo "built binary lacks with_v2ray_api" >&2; exit 3; }
`

// Build runs the container and returns the artifact it produced.
func (b *DockerBuilder) Build(ctx context.Context, core, version, arch, destDir string) (string, int64, string, error) {
	if core != "singbox" {
		return "", 0, "", fmt.Errorf("no build recipe for core %q", core)
	}
	name := fmt.Sprintf("sing-box-%s-metred-linux-%s", version, arch)
	out := filepath.Join(destDir, name)
	_ = os.Remove(out)
	// Reuse one module/build cache across versions: a rebuild then takes seconds
	// instead of re-downloading the whole dependency tree.
	cache := filepath.Join(filepath.Dir(destDir), "gocache")
	_ = os.MkdirAll(cache, 0o755)

	script := buildScript
	bctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	cmd := exec.CommandContext(bctx, "docker", "run", "--rm",
		"--network", b.Network,
		"-e", "VER="+version,
		"-e", "GOOS=linux",
		"-e", "GOARCH="+arch,
		"-e", "CGO_ENABLED=0",
		"-v", destDir+":/out",
		"-v", cache+":/go",
		b.Image, "bash", "-c", script)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(outBytes))
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return "", 0, "", fmt.Errorf("docker build failed: %v: %s", err, msg)
	}
	// The container writes /out/sing-box; move it to the versioned name the
	// catalog and the download route expect.
	produced := filepath.Join(destDir, "sing-box")
	if _, statErr := os.Stat(out); statErr != nil {
		if renErr := os.Rename(produced, out); renErr != nil {
			return "", 0, "", fmt.Errorf("build produced no artifact at %s: %v", out, renErr)
		}
	}
	info, err := os.Stat(out)
	if err != nil {
		return "", 0, "", fmt.Errorf("build produced no artifact: %w", err)
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", 0, "", err
	}
	sum, err := fileSHA256(out)
	if err != nil {
		return "", 0, "", err
	}
	return name, info.Size(), sum, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
