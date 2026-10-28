package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vortexui/vortexui/internal/config"
)

// InstallCore replaces one core's binary with an artifact served by the panel.
//
// The order is deliberate: download to a temp file in the target directory, check
// the checksum, keep the current binary as .bak, swap atomically, then prove the
// new binary runs. Any failure restores the old binary, so a bad upgrade can
// never leave a node without a working core.
func InstallCore(ctx context.Context, cfg *config.Node, core, version, url, wantSHA string, allowUnverified bool, log *slog.Logger) error {
	if cfg == nil {
		return fmt.Errorf("no node config")
	}
	core = strings.TrimSpace(core)
	wantSHA = strings.ToLower(strings.TrimSpace(wantSHA))
	if core != "xray" && core != "singbox" {
		return fmt.Errorf("unknown core %q", core)
	}
	if wantSHA == "" && !allowUnverified {
		return fmt.Errorf("refusing an unverified artifact: sha256 is required")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("unsupported artifact url %q", url)
	}
	bin := cfg.CoreBinFor(core)
	dir := filepath.Dir(bin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".vortex-core-install-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	log.Info("installing core", "core", core, "version", version, "url", url)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: http %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && got != wantSHA {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantSHA)
	}
	if wantSHA == "" {
		log.Warn("installing an unverified vendor artifact", "core", core, "version", version, "sha256", got)
	}

	// Vendor artifacts are archives; panel builds are raw binaries.
	binaryPath, err := extractCoreBinary(tmpName, dir, core)
	if err != nil {
		return err
	}
	if binaryPath == "" {
		binaryPath = tmpName
	}
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		return err
	}

	// Prove the artifact runs before it becomes the live core.
	if out, err := exec.CommandContext(dctx, binaryPath, "version").CombinedOutput(); err != nil {
		return fmt.Errorf("artifact does not run: %v (%s)", err, firstLine(string(out)))
	}

	backup := bin + ".bak"
	hadOld := false
	if _, err := os.Stat(bin); err == nil {
		if err := copyFile(bin, backup); err != nil {
			return fmt.Errorf("backup current core: %w", err)
		}
		hadOld = true
	}
	if err := os.Rename(binaryPath, bin); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		if hadOld {
			_ = os.Rename(backup, bin)
		}
		return err
	}
	// CommandContext already ensured the binary starts; a failure right here is
	// almost always a permissions or filesystem problem, so roll back.
	if ok, why := binaryUsable(dctx, bin); !ok {
		if hadOld {
			_ = os.Rename(backup, bin)
		}
		return fmt.Errorf("installed binary unusable (%s); rolled back", why)
	}
	log.Info("core installed", "core", core, "version", version, "path", bin, "backup", backup)
	return nil
}
