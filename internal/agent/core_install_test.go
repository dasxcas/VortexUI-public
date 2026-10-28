package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/vortexui/vortexui/internal/config"
)

func testNode(t *testing.T, binBody string) (*config.Node, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "xray")
	if binBody != "" {
		if err := os.WriteFile(bin, []byte(binBody), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Node{Core: "xray", CoreBin: bin, EnabledCores: []string{"xray"}}, bin
}

func serve(t *testing.T, body []byte) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	return srv, hex.EncodeToString(sum[:])
}

func discardingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const goodScript = "#!/bin/sh\necho 'Xray 26.1.1'\n"

// TestInstallCoreReplacesBinary proves the happy path: checksum verified, old
// binary preserved as .bak, new one live and executable.
func TestInstallCoreReplacesBinary(t *testing.T) {
	cfg, bin := testNode(t, "#!/bin/sh\necho 'Xray 1.0.0'\n")
	srv, sum := serve(t, []byte(goodScript))

	if err := InstallCore(context.Background(), cfg, "xray", "26.1.1", srv.URL, sum, false, discardingLogger()); err != nil {
		t.Fatalf("install: %v", err)
	}
	body, _ := os.ReadFile(bin)
	if string(body) != goodScript {
		t.Fatalf("binary not replaced: %q", body)
	}
	if _, err := os.Stat(bin + ".bak"); err != nil {
		t.Fatal("previous binary was not kept as a rollback target")
	}
}

// TestInstallCoreRejectsBadChecksum proves a corrupted download never touches
// the live binary.
func TestInstallCoreRejectsBadChecksum(t *testing.T) {
	cfg, bin := testNode(t, "#!/bin/sh\necho 'Xray 1.0.0'\n")
	before, _ := os.ReadFile(bin)
	srv, _ := serve(t, []byte(goodScript))

	err := InstallCore(context.Background(), cfg, "xray", "26.1.1", srv.URL, "0000deadbeef", false, discardingLogger())
	if err == nil {
		t.Fatal("expected checksum failure")
	}
	after, _ := os.ReadFile(bin)
	if string(after) != string(before) {
		t.Fatal("binary changed despite a failed checksum")
	}
}

// TestInstallCoreRequiresChecksumUnlessAllowed proves the default posture: an
// artifact with no checksum is refused unless the caller explicitly opts in.
func TestInstallCoreRequiresChecksumUnlessAllowed(t *testing.T) {
	cfg, _ := testNode(t, "old")
	srv, _ := serve(t, []byte(goodScript))

	if err := InstallCore(context.Background(), cfg, "xray", "26.1.1", srv.URL, "", false, discardingLogger()); err == nil {
		t.Fatal("expected refusal without a checksum")
	}
	if err := InstallCore(context.Background(), cfg, "xray", "26.1.1", srv.URL, "", true, discardingLogger()); err != nil {
		t.Fatalf("explicitly unverified install should proceed: %v", err)
	}
}

// TestInstallCoreRejectsUnrunnableArtifact proves a downloaded file that is not
// a working binary is caught before the swap.
func TestInstallCoreRejectsUnrunnableArtifact(t *testing.T) {
	cfg, bin := testNode(t, "#!/bin/sh\necho 'Xray 1.0.0'\n")
	before, _ := os.ReadFile(bin)
	srv, sum := serve(t, []byte("this is not an executable\n"))

	if err := InstallCore(context.Background(), cfg, "xray", "26.1.1", srv.URL, sum, false, discardingLogger()); err == nil {
		t.Fatal("expected a failure for a non-executable artifact")
	}
	after, _ := os.ReadFile(bin)
	if string(after) != string(before) {
		t.Fatal("binary was replaced by an unusable artifact")
	}
}

// TestInstallCoreUnknownCore proves a typo cannot point the installer at an
// arbitrary file.
func TestInstallCoreUnknownCore(t *testing.T) {
	cfg, _ := testNode(t, "old")
	srv, sum := serve(t, []byte(goodScript))
	if err := InstallCore(context.Background(), cfg, "shadow", "1", srv.URL, sum, false, discardingLogger()); err == nil {
		t.Fatal("expected unknown core to be refused")
	}
}
