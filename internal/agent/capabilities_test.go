package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/vortexui/vortexui/internal/config"
)

func singboxNode(t *testing.T, body string) (*config.Node, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sing-box")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Node{
		Core:         "singbox",
		EnabledCores: []string{"singbox"},
		CoreBin:      bin,
		CoreConfig:   filepath.Join(dir, "core.json"),
	}, bin
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestStockBinaryDisablesStatsEvenWhenEnvSaysOtherwise is the whole point: an
// operator upgrading to a vendor build must not end up with a config the binary
// rejects. The binary gets the final word.
func TestStockBinaryDisablesStatsEvenWhenEnvSaysOtherwise(t *testing.T) {
	t.Setenv("VORTEX_SINGBOX_V2RAY_API", "true")
	cfg, _ := singboxNode(t, "#!/bin/sh\necho 'sing-box version v1.14.4'\necho 'Tags: with_quic,with_gvisor'\n")

	if !cfg.SingboxV2RayAPIFor() {
		t.Fatal("precondition: env asks for stats")
	}
	ApplyCoreCapabilities(context.Background(), cfg, quiet())
	if cfg.SingboxV2RayAPIFor() {
		t.Fatal("a build without with_v2ray_api must not be handed a v2ray_api block")
	}
}

// TestMeteredBinaryKeepsStats proves the probe does not disable stats on the
// baseline build.
func TestMeteredBinaryKeepsStats(t *testing.T) {
	t.Setenv("VORTEX_SINGBOX_V2RAY_API", "false")
	cfg, _ := singboxNode(t, "#!/bin/sh\necho 'sing-box version v1.14.2'\necho 'Tags: with_v2ray_api'\n")

	ApplyCoreCapabilities(context.Background(), cfg, quiet())
	if !cfg.SingboxV2RayAPIFor() {
		t.Fatal("a metered build should get the stats block even if env said false")
	}
}

// TestCapabilityChangeDetected covers the restart trigger: swapping one binary
// for another flips the answer.
func TestCapabilityChangeDetected(t *testing.T) {
	t.Setenv("VORTEX_SINGBOX_V2RAY_API", "true")
	cfg, bin := singboxNode(t, "#!/bin/sh\necho 'sing-box version v1.14.2'\necho 'Tags: with_v2ray_api'\n")

	ApplyCoreCapabilities(context.Background(), cfg, quiet())
	if MeteringCapabilityChanged(context.Background(), cfg) {
		t.Fatal("nothing changed yet")
	}
	// Replace the binary with an unmetered one, as an install would.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'sing-box version v1.14.4'\necho 'Tags: with_quic'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !MeteringCapabilityChanged(context.Background(), cfg) {
		t.Fatal("expected the agent to notice the new binary cannot meter")
	}
}

// TestUnreadableBinaryIsNotAVerdict proves a missing or broken binary leaves the
// decision alone instead of silently disabling stats.
func TestUnreadableBinaryIsNotAVerdict(t *testing.T) {
	cfg := &config.Node{Core: "singbox", EnabledCores: []string{"singbox"}, CoreBin: filepath.Join(t.TempDir(), "missing")}
	ApplyCoreCapabilities(context.Background(), cfg, quiet())
	if _, known := MeteringCapability(context.Background(), cfg); known {
		t.Fatal("missing binary must report unknown, not metered")
	}
	if MeteringCapabilityChanged(context.Background(), cfg) {
		t.Fatal("unknown capability must not trigger a restart")
	}
}
