package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/vortexui/vortexui/internal/config"
)

// ApplyCoreCapabilities makes the rendered config follow the binary that is
// actually on disk, not the environment.
//
// The failure this prevents is asymmetric and easy to miss: a sing-box build
// without with_v2ray_api rejects the experimental.v2ray_api block, so a node
// whose environment still says "we meter" writes a config its own core refuses,
// and goes from "not counting" to "not running". Trusting the binary means an
// upgrade can never take the node down, it only stops the counters.
func ApplyCoreCapabilities(ctx context.Context, cfg *config.Node, log *slog.Logger) {
	if cfg == nil || !coreEnabled(cfg, "singbox") {
		return
	}
	capable, known := MeteringCapability(ctx, cfg)
	if !known {
		return
	}
	cfg.SetMeteringCapability(capable)
	if !capable {
		log.Warn("sing-box build has no with_v2ray_api: rendering config without per-user stats",
			"bin", cfg.CoreBinFor("singbox"), "consequence", "traffic accounting stays at zero on this node")
	}
}

// MeteringCapability reports whether this node's sing-box can attribute traffic
// per user. known is false when the binary cannot be inspected (missing, or it
// does not print a Tags line), in which case callers must not guess.
func MeteringCapability(ctx context.Context, cfg *config.Node) (capable, known bool) {
	if cfg == nil {
		return false, false
	}
	bin := cfg.CoreBinFor("singbox")
	tags := coreTags(ctx, bin)
	if tags == "" {
		return false, false
	}
	return strings.Contains(tags, "with_v2ray_api"), true
}

// MeteringCapabilityChanged reports whether the binary on disk now disagrees
// with what this process was started with — the signal to restart the agent so
// the driver rebuilds its config builder.
func MeteringCapabilityChanged(ctx context.Context, cfg *config.Node) bool {
	if cfg == nil || !coreEnabled(cfg, "singbox") {
		return false
	}
	capable, known := MeteringCapability(ctx, cfg)
	if !known {
		return false
	}
	return capable != cfg.SingboxV2RayAPIFor()
}

// coreEnabled reports whether this node is supposed to run the given engine.
func coreEnabled(cfg *config.Node, core string) bool {
	if len(cfg.EnabledCores) == 0 {
		return cfg.Core == core
	}
	for _, c := range cfg.EnabledCores {
		if c == core {
			return true
		}
	}
	return false
}
