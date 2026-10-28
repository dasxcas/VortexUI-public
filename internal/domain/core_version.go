package domain

import (
	"time"

	"github.com/google/uuid"
)

// CoreSource says where a core binary came from.
type CoreSource string

const (
	// CoreFromUpstream is the vendor's own release. For sing-box that build has
	// no with_v2ray_api, so it cannot meter traffic per user.
	CoreFromUpstream CoreSource = "upstream"
	// CoreBundled ships inside the panel image. It is the metering fallback and is
	// never offered for deletion.
	CoreBundled CoreSource = "bundled"
	// CoreFromBuild is an artifact this panel compiled itself.
	CoreFromBuild CoreSource = "built"
	// CoreFromUpload is an operator-supplied binary.
	CoreFromUpload CoreSource = "uploaded"
)

// CoreBuildStatus tracks the lifecycle of a catalog entry.
type CoreBuildStatus string

const (
	CoreReady    CoreBuildStatus = "ready"
	CoreBuilding CoreBuildStatus = "building"
	CoreFailed   CoreBuildStatus = "failed"
)

// CoreVersion is one (core, version, arch) artifact the panel knows about.
type CoreVersion struct {
	ID      uuid.UUID       `json:"id"`
	Core    string          `json:"core"` // xray | singbox
	Version string          `json:"version"`
	Arch    string          `json:"arch"`
	Source  CoreSource      `json:"source"`
	Status  CoreBuildStatus `json:"status"`
	// Metered reports whether this build can count traffic per user. It is set
	// from the artifact's own build tags, never assumed.
	Metered bool `json:"metered"`

	URL       string `json:"url,omitempty"`
	File      string `json:"file,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Error     string `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Usable reports whether the artifact can be installed on a node right now.
func (c *CoreVersion) Usable() bool {
	return c != nil && c.Status == CoreReady && c.URL != ""
}

// Verified reports whether the artifact has a checksum the agent can enforce.
// Vendor releases have none, so installing one requires an explicit opt-in.
func (c *CoreVersion) Verified() bool {
	return c != nil && c.SHA256 != ""
}
