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
)

// UpgradeSelf replaces the running agent binary with the one the panel serves.
//
// The order mirrors InstallCore because the failure mode is worse here: if the
// swap leaves a broken binary and the process restarts, the node is unreachable
// and needs hands on it. So the new artifact is downloaded next to the current
// executable, checksummed, and executed once ("version") before it is allowed to
// replace anything.
func UpgradeSelf(ctx context.Context, url, wantSHA string, log *slog.Logger) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("no agent artifact url")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)

	tmp, err := os.CreateTemp(dir, ".vortex-agent-upgrade-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
	if wantSHA != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, wantSHA) {
			return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantSHA)
		}
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	// Prove it runs before it becomes the binary systemd will start.
	if out, err := exec.CommandContext(dctx, tmpName, "version").CombinedOutput(); err != nil {
		return fmt.Errorf("new agent does not run: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	backup := exe + ".bak"
	if err := copyFile(exe, backup); err != nil {
		return fmt.Errorf("backup current agent: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("replace agent: %w", err)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		_ = os.Rename(backup, exe)
		return err
	}
	log.Info("agent binary replaced", "path", exe, "backup", backup)
	return nil
}
