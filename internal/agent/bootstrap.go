package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
	"time"

	"github.com/vortexui/vortexui/internal/config"
)

// Core binaries are not part of the panel or the agent, and asking an operator
// to SSH in and install one by hand is exactly the step a control plane should
// remove. EnsureCores checks each enabled core and, when the binary is missing,
// fetches the official release and installs it in place.

// autoInstallEnabled reports whether the agent may download cores itself. On by
// default; VORTEX_AUTO_INSTALL_CORE=false pins the environment instead.
func autoInstallEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_AUTO_INSTALL_CORE")))
	switch v {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// CoreReport is what the agent learned about one core at startup.
type CoreReport struct {
	Core    string
	Bin     string
	Version string
	Tags    string
	Metered bool // whether per-user traffic can be counted with this build
}

// EnsureCores makes every enabled core runnable, downloading what is missing.
// It never returns an error: a node with one broken core must still come up so
// the panel can show the problem instead of the node vanishing.
func EnsureCores(ctx context.Context, cfg *config.Node, log *slog.Logger) []CoreReport {
	if cfg == nil || log == nil {
		return nil
	}
	var reports []CoreReport
	cores := cfg.EnabledCores
	if len(cores) == 0 {
		cores = []string{cfg.Core}
	}
	for _, core := range cores {
		core = strings.TrimSpace(core)
		if core == "" {
			continue
		}
		bin := cfg.CoreBinFor(core)
		if ok, why := binaryUsable(ctx, bin); ok {
			log.Info("core binary ready", "core", core, "path", bin)
			warnMissingStatsTag(ctx, core, bin, cfg, log)
			reports = append(reports, describeCore(ctx, core, bin))
			continue
		} else if why != "" {
			log.Warn("core binary unusable", "core", core, "path", bin, "reason", why)
		}
		// Refuse to install the stock sing-box on a node that must meter traffic:
		// the official build has no with_v2ray_api, so it would come up counting
		// zero. Better to fail here, loudly, than to serve unmetered traffic.
		if core == "singbox" && cfg.SingboxV2RayAPI && strings.TrimSpace(os.Getenv("VORTEX_SINGBOX_URL")) == "" {
			log.Error("refusing to install stock sing-box on a node that must meter traffic",
				"core", core, "path", bin,
				"hint", "set VORTEX_SINGBOX_URL to a build compiled with -tags with_v2ray_api")
			reports = append(reports, CoreReport{Core: core, Bin: bin, Metered: false})
			continue
		}
		if !autoInstallEnabled() {
			log.Warn("core binary missing and auto-install is disabled",
				"core", core, "path", bin, "hint", "install it or set VORTEX_AUTO_INSTALL_CORE=true")
			continue
		}
		if err := installCore(ctx, core, bin, log); err != nil {
			log.Error("core auto-install failed", "core", core, "path", bin, "err", err)
			continue
		}
		if ok, why := binaryUsable(ctx, bin); !ok {
			log.Error("core installed but not runnable", "core", core, "path", bin, "reason", why)
			continue
		}
		log.Info("core installed", "core", core, "path", bin)
		warnMissingStatsTag(ctx, core, bin, cfg, log)
		reports = append(reports, describeCore(ctx, core, bin))
	}
	return reports
}

// describeCore records the version and build tags of a usable binary, plus
// whether that build can meter traffic per user.
func describeCore(ctx context.Context, core, bin string) CoreReport {
	rep := CoreReport{Core: core, Bin: bin}
	rep.Version, rep.Tags = coreVersionTags(ctx, bin)
	// Xray has the stats API built in; sing-box only when compiled with it.
	rep.Metered = core != "singbox" || strings.Contains(rep.Tags, "with_v2ray_api")
	return rep
}

// coreVersionTags parses the `version` output of either core.
func coreVersionTags(ctx context.Context, bin string) (version, tags string) {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, bin, "version").CombinedOutput()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Tags:") {
			tags = strings.TrimSpace(strings.TrimPrefix(line, "Tags:"))
			continue
		}
		if version != "" {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 3 && fields[0] == "sing-box" && fields[1] == "version":
			version = fields[2]
		case len(fields) >= 2 && strings.EqualFold(fields[0], "xray"):
			version = fields[1]
		}
	}
	return version, tags
}

// warnMissingStatsTag catches the quiet failure that costs an operator days:
// the official sing-box build has no with_v2ray_api tag, so the per-user stats
// block is never emitted and traffic reads as zero. Say so at startup instead.
func warnMissingStatsTag(ctx context.Context, core, bin string, cfg *config.Node, log *slog.Logger) {
	if core != "singbox" || !cfg.SingboxV2RayAPI {
		return
	}
	if strings.Contains(coreTags(ctx, bin), "with_v2ray_api") {
		return
	}
	log.Warn("sing-box binary lacks with_v2ray_api: per-user traffic will read as zero",
		"core", core, "path", bin,
		"fix", "use a build tagged with_v2ray_api, or set VORTEX_SINGBOX_V2RAY_API=false to accept global-only stats")
}

// coreTags returns the "Tags:" line that both cores print with `version`.
func coreTags(ctx context.Context, bin string) string {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, bin, "version").CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Tags:") {
			return line
		}
	}
	return ""
}

// binaryUsable reports whether the path is an executable that answers --version.
// Running it catches the truncated or wrong-architecture download that a plain
// stat would accept.
func binaryUsable(ctx context.Context, bin string) (bool, string) {
	info, err := os.Stat(bin)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "not found"
		}
		return false, err.Error()
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 || info.Size() == 0 {
		return false, "not an executable file"
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, bin, "version").CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("version check failed: %v (%s)", err, firstLine(string(out)))
	}
	return true, ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// downloadPlan describes where a core's release lives.
type downloadPlan struct {
	url   string
	kind  string // "zip" or "tar.gz"
	files []string
}

// planFor builds the download plan for an official release. Xray publishes a
// stable "latest" URL; sing-box names its assets by version, so the tag has to
// be resolved first.
func planFor(ctx context.Context, core, arch string) (downloadPlan, error) {
	switch core {
	case "xray":
		asset, ok := map[string]string{"amd64": "64", "arm64": "arm64-v8a", "386": "32", "arm": "arm32-v7a"}[arch]
		if !ok {
			return downloadPlan{}, fmt.Errorf("xray: unsupported architecture %q", arch)
		}
		return downloadPlan{
			url:   "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-" + asset + ".zip",
			kind:  "zip",
			files: []string{"xray", "geoip.dat", "geosite.dat"},
		}, nil
	case "singbox":
		suffix, ok := map[string]string{"amd64": "amd64", "arm64": "arm64", "386": "386", "arm": "armv7"}[arch]
		if !ok {
			return downloadPlan{}, fmt.Errorf("sing-box: unsupported architecture %q", arch)
		}
		// A prebuilt binary wins over the upstream release: the official build
		// lacks with_v2ray_api, so per-user stats need a custom artifact.
		if raw := strings.TrimSpace(os.Getenv("VORTEX_SINGBOX_URL")); raw != "" {
			kind := "raw"
			switch {
			case strings.HasSuffix(raw, ".tar.gz"), strings.HasSuffix(raw, ".tgz"):
				kind = "tar.gz"
			case strings.HasSuffix(raw, ".gz"):
				kind = "gz"
			}
			return downloadPlan{url: raw, kind: kind, files: []string{"sing-box"}}, nil
		}
		ver, err := latestTag(ctx, "https://api.github.com/repos/SagerNet/sing-box/releases/latest")
		if err != nil {
			return downloadPlan{}, err
		}
		v := strings.TrimPrefix(ver, "v")
		return downloadPlan{
			url:   fmt.Sprintf("https://github.com/SagerNet/sing-box/releases/download/%s/sing-box-%s-linux-%s.tar.gz", ver, v, suffix),
			kind:  "tar.gz",
			files: []string{"sing-box"},
		}, nil
	default:
		return downloadPlan{}, fmt.Errorf("no release source for core %q", core)
	}
}

func latestTag(ctx context.Context, apiURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve latest version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve latest version: http %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", errors.New("resolve latest version: empty tag")
	}
	return body.TagName, nil
}

// installCore downloads, unpacks and installs one core. The binary is moved into
// place atomically so a half-written download can never become the running core.
func installCore(ctx context.Context, core, bin string, log *slog.Logger) error {
	plan, err := planFor(ctx, core, runtime.GOARCH)
	if err != nil {
		return err
	}
	// The archive member is named after the product (sing-box), not after the
	// internal core id (singbox).
	binName := binaryName(core)
	dir := filepath.Dir(bin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("prepare %s: %w", dir, err)
	}
	// Fail before downloading when the target directory is not writable: the
	// message should say "run as root", not "permission denied" after 20 MB.
	probe, err := os.CreateTemp(dir, ".vortex-write-test-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (run the agent as root or point VORTEX_%s_BIN elsewhere): %w",
			dir, strings.ToUpper(core), err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	log.Info("downloading core", "core", core, "url", plan.url)
	archive, err := download(ctx, plan.url)
	if err != nil {
		return err
	}
	defer os.Remove(archive)

	// A pinned checksum turns "some server served me a binary" into a decision
	// the operator made in advance.
	if want := strings.TrimSpace(os.Getenv("VORTEX_" + strings.ToUpper(core) + "_SHA256")); want != "" {
		if err := verifyChecksum(archive, want); err != nil {
			return err
		}
	}

	members, err := extract(ctx, plan.kind, archive, dir, plan.files, core)
	if err != nil {
		return err
	}
	src, ok := members[binName]
	if !ok {
		return fmt.Errorf("%s: archive did not contain a %s binary", core, binName)
	}
	if err := os.Chmod(src, 0o755); err != nil {
		return err
	}
	if src != bin {
		if err := os.Rename(src, bin); err != nil {
			// Cross-device or busy: fall back to copy.
			if err := copyFile(src, bin); err != nil {
				return err
			}
			_ = os.Remove(src)
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// binaryName is the executable name inside the release archive.
func binaryName(core string) string {
	switch core {
	case "singbox":
		return "sing-box"
	default:
		return core
	}
}

func download(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: http %d", resp.StatusCode)
	}
	f, err := os.CreateTemp("", "vortex-core-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// extract unpacks the wanted members into dir and returns their paths by name.
func extract(_ context.Context, kind, archive, dir string, wanted []string, core string) (map[string]string, error) {
	want := map[string]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	out := map[string]string{}
	switch kind {
	case "zip":
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return nil, fmt.Errorf("open zip: %w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			name := filepath.Base(f.Name)
			if !want[name] {
				continue
			}
			dest := filepath.Join(dir, name)
			if err := writeZipMember(f, dest); err != nil {
				return nil, err
			}
			out[name] = dest
		}
	case "tar.gz":
		f, err := os.Open(archive)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := filepath.Base(hdr.Name)
			if hdr.Typeflag != tar.TypeReg || !want[name] {
				continue
			}
			dest := filepath.Join(dir, name)
			if err := writeStream(tr, dest); err != nil {
				return nil, err
			}
			if err := os.Chmod(dest, 0o755); err != nil {
				return nil, err
			}
			out[name] = dest
		}
	case "raw":
		// A prebuilt binary served as-is.
		if err := copyFile(archive, filepath.Join(dir, "sing-box")); err != nil {
			return nil, err
		}
		out["sing-box"] = filepath.Join(dir, "sing-box")
	case "gz":
		f, err := os.Open(archive)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer gz.Close()
		dest := filepath.Join(dir, "sing-box")
		if err := writeStream(gz, dest); err != nil {
			return nil, err
		}
		out["sing-box"] = dest
	default:
		return nil, fmt.Errorf("unsupported archive %q", kind)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: nothing extracted from archive", core)
	}
	return out, nil
}

func writeZipMember(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeStream(rc, dest)
}

func writeStream(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".vortex-core-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, dest)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".vortex-core-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, dst)
}

// verifyChecksum rejects a download that does not match the operator's pin.
func verifyChecksum(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(want)) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}
