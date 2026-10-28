package singbox

import (
	"encoding/json"
	"testing"

	"github.com/vortexui/vortexui/internal/core"
)

// render builds a minimal config and returns it as a map for inspection.
func render(t *testing.T, omit bool) map[string]any {
	t.Helper()
	b := Builder{APIPort: 10085, OmitV2RayAPI: omit}
	raw, err := b.Build(&core.GeneratedConfig{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("rendered output is not JSON: %v (%s)", err, raw)
	}
	return out
}

// TestOmitV2RayAPIRemovesTheBlock is the shape guarantee the installer relies on:
// a stock binary is never handed a field it rejects.
func TestOmitV2RayAPIRemovesTheBlock(t *testing.T) {
	with := render(t, false)
	without := render(t, true)

	exp, ok := with["experimental"].(map[string]any)
	if !ok {
		t.Fatal("metered build should emit an experimental block")
	}
	if _, ok := exp["v2ray_api"]; !ok {
		t.Fatal("metered build should emit v2ray_api")
	}

	if raw, ok := without["experimental"]; ok {
		if exp, ok := raw.(map[string]any); ok {
			if _, has := exp["v2ray_api"]; has {
				t.Fatal("v2ray_api must be omitted for a build without the tag")
			}
		}
	}
}
