package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vortexui/vortexui/internal/config"
)

// TestBinaryUsable separates "missing" from "present but broken" so the agent
// reinstalls for the right reason and logs something actionable.
func TestBinaryUsable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho fake 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why := binaryUsable(context.Background(), good); !ok {
		t.Fatalf("executable script should be usable: %s", why)
	}

	if ok, why := binaryUsable(context.Background(), filepath.Join(dir, "nope")); ok || why != "not found" {
		t.Fatalf("missing binary: ok=%v why=%q", ok, why)
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, why := binaryUsable(context.Background(), plain); ok {
		t.Fatalf("non-executable file must be rejected (why=%q)", why)
	}
}

// TestPlanForXray checks the release URL shape per architecture: a wrong suffix
// silently downloads a binary for another CPU.
func TestPlanForXray(t *testing.T) {
	cases := map[string]string{
		"amd64": "Xray-linux-64.zip",
		"arm64": "Xray-linux-arm64-v8a.zip",
		"arm":   "Xray-linux-arm32-v7a.zip",
		"386":   "Xray-linux-32.zip",
	}
	for arch, want := range cases {
		plan, err := planFor(context.Background(), "xray", arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if !strings.HasSuffix(plan.url, want) || plan.kind != "zip" {
			t.Fatalf("%s: url=%q kind=%q", arch, plan.url, plan.kind)
		}
	}
	if _, err := planFor(context.Background(), "xray", "mips"); err == nil {
		t.Fatal("unsupported architecture must be rejected")
	}
	if _, err := planFor(context.Background(), "singbox", "mips"); err == nil {
		t.Fatal("unsupported architecture must be rejected for sing-box too")
	}
	if _, err := planFor(context.Background(), "v2ray", "amd64"); err == nil {
		t.Fatal("unknown core must be rejected")
	}
}

// TestBinaryName covers the archive/product name mismatch that broke the first
// auto-install: the id is singbox, the executable is sing-box.
func TestBinaryName(t *testing.T) {
	if binaryName("singbox") != "sing-box" {
		t.Fatal("singbox must map to sing-box")
	}
	if binaryName("xray") != "xray" {
		t.Fatal("xray must map to xray")
	}
}

// TestEnsureCoresRespectsOptOut proves the agent does not reach the network when
// the operator pinned the environment.
func TestEnsureCoresRespectsOptOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VORTEX_AUTO_INSTALL_CORE", "false")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Node{
		Core:       "xray",
		CoreBin:    filepath.Join(dir, "xray"),
		EnabledCores: []string{"xray"},
	}
	EnsureCores(context.Background(), cfg, log)
	if _, err := os.Stat(cfg.CoreBin); !os.IsNotExist(err) {
		t.Fatal("auto-install must not write anything when disabled")
	}
}

// TestEnsureCoresLeavesExistingBinaryAlone proves a working core is not
// reinstalled on every start.
func TestEnsureCoresLeavesExistingBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "xray")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho xray 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	EnsureCores(context.Background(), &config.Node{Core: "xray", CoreBin: bin, EnabledCores: []string{"xray"}}, log)
	after, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("existing binary must not be touched")
	}
}

// TestPlanForSingboxPrefersPrebuiltURL: an operator-hosted binary replaces the
// upstream release, and the archive kind follows the file name so a raw binary,
// a .gz or a .tar.gz all install correctly.
func TestPlanForSingboxPrefersPrebuiltURL(t *testing.T) {
	cases := []struct{ url, kind string }{
		{"https://cdn.example.com/sing-box-linux-amd64", "raw"},
		{"https://cdn.example.com/sing-box-linux-amd64.gz", "gz"},
		{"https://cdn.example.com/sing-box-1.14.4-linux-amd64.tar.gz", "tar.gz"},
	}
	for _, c := range cases {
		t.Setenv("VORTEX_SINGBOX_URL", c.url)
		plan, err := planFor(context.Background(), "singbox", "amd64")
		if err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		if plan.url != c.url || plan.kind != c.kind {
			t.Fatalf("%s: got url=%q kind=%q, want kind=%q", c.url, plan.url, plan.kind, c.kind)
		}
	}
	t.Setenv("VORTEX_SINGBOX_URL", "")
}

// TestVerifyChecksum proves a tampered or wrong-architecture download is refused
// before it is installed.
func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "binary")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	const sum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := verifyChecksum(p, sum); err != nil {
		t.Fatalf("correct checksum rejected: %v", err)
	}
	if err := verifyChecksum(p, "deadbeef"); err == nil {
		t.Fatal("mismatched checksum must be rejected")
	}
}

// TestEnsureCoresRefusesStockSingboxWhenMetering: a node that must count traffic
// per user must not install the official build, which cannot. Fail loudly
// instead of serving traffic nobody is billed for.
func TestEnsureCoresRefusesStockSingboxWhenMetering(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VORTEX_SINGBOX_URL", "")
	t.Setenv("VORTEX_AUTO_INSTALL_CORE", "true")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Node{
		Core:            "singbox",
		CoreBin:         filepath.Join(dir, "sing-box"),
		EnabledCores:    []string{"singbox"},
		SingboxV2RayAPI: true,
	}
	reports := EnsureCores(context.Background(), cfg, log)
	if _, err := os.Stat(cfg.CoreBin); !os.IsNotExist(err) {
		t.Fatal("stock sing-box must not be installed when metering is required")
	}
	if len(reports) != 1 || reports[0].Metered {
		t.Fatalf("report must record an unmetered core, got %+v", reports)
	}
}
