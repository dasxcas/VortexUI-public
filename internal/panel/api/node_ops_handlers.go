package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"fmt"
	"github.com/vortexui/vortexui/internal/domain"
	"strings"
)

func enrichNode(n *domain.Node, h *Handlers) {
	if n == nil || h.Hub == nil {
		return
	}
	st, hl, d, ok := h.Hub.Live(n.ID)
	if !ok {
		return
	}
	n.Status = st
	n.Health = hl
	dd := d
	n.Diagnostics = &dd
	n.EnrollmentPhase = enrollmentPhase(n)
}

func enrollmentPhase(n *domain.Node) domain.NodeEnrollmentPhase {
	if n.Diagnostics == nil {
		return domain.NodePhasePending
	}
	switch n.Diagnostics.Code {
	case domain.NodeDiagOK:
		if n.LastSeen != nil && time.Since(*n.LastSeen) <= 90*time.Second {
			return domain.NodePhaseSynced
		}
		return domain.NodePhaseConnected
	case domain.NodeDiagCoreDown:
		return domain.NodePhaseConnected
	case domain.NodeDiagMTLS:
		if n.Diagnostics.NetworkReachable {
			return domain.NodePhasePending
		}
	}
	if n.Diagnostics.CAMatch && n.Diagnostics.NetworkReachable {
		return domain.NodePhaseConnected
	}
	if n.Status == domain.NodeConnected {
		return domain.NodePhaseConnected
	}
	return domain.NodePhasePending
}

// GetNodeEnrollment returns the mTLS enrollment bundle for adding a remote node.
// With ?node_id=<uuid> it issues (or reuses) that node's own identity; without
// it the legacy shared bundle is returned for backwards compatibility.
func (h *Handlers) GetNodeEnrollment(c echo.Context) error {
	if raw := c.QueryParam("node_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid node_id")
		}
		return h.enrollNode(c, id)
	}
	if h.Enrollment == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "enrollment not configured")
	}
	b, err := h.Enrollment.Bundle()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, b)
}

// GetNodeEnrollmentFor issues the node's own certificate and returns its bundle.
func (h *Handlers) GetNodeEnrollmentFor(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	return h.enrollNode(c, id)
}

// RevokeNodeCert used to invalidate the node's identity without issuing a
// replacement. That left the row revoked with no fingerprint, and the dialer
// then fell back to CA-only verification — so the certificate the caller meant
// to kill kept working. There is no longer any way to express that state: this
// endpoint now performs a rotation and says so, and new callers should use
// POST /api/nodes/:id/cert/rotate.
func (h *Handlers) RevokeNodeCert(c echo.Context) error {
	c.Response().Header().Set("Deprecation", "true")
	c.Response().Header().Set("Link", "</api/nodes/"+c.Param("id")+"/cert/rotate>; rel=\"successor-version\"")
	slog.Warn("deprecated cert/revoke called; rotating instead of leaving a bare revocation",
		"node_id", c.Param("id"))
	return h.rotateNodeCert(c, true)
}

// RotateNodeCert replaces the node's identity in one step and returns the new
// bundle, so revoking and re-issuing never leaves a gap.
func (h *Handlers) RotateNodeCert(c echo.Context) error {
	return h.rotateNodeCert(c, false)
}

func (h *Handlers) rotateNodeCert(c echo.Context, deprecated bool) error {
	if h.NodeCerts == nil || h.Enrollment == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "node certificates not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	ctx := c.Request().Context()
	n, err := h.Nodes.Get(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "node not found")
	}
	// Preferred path: hand the identity to the agent over its existing mTLS
	// channel so it swaps the certificate in place. Falls back to the manual
	// revoke-and-download flow when the node is unreachable or runs an agent
	// that predates this RPC.
	var pushErr error
	if cert, pushed, err := h.NodeCerts.RotatePush(ctx, n); err == nil && pushed {
		b, berr := h.Enrollment.BundleFor(cert)
		if berr != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, berr.Error())
		}
		return c.JSON(http.StatusOK, echo.Map{
			"bundle":         b.Bundle,
			"ca_fingerprint": b.CAFingerprint,
			"cert_dir":       b.CertDir,
			"node_id":        n.ID,
			"fingerprint":    cert.Fingerprint,
			"expires_at":     cert.ExpiresAt,
			"rotated":        true,
			"pushed":         true,
		})
	} else if err != nil {
		pushErr = err
	}
	cert, err := h.NodeCerts.Rotate(ctx, n)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	b, err := h.Enrollment.BundleFor(cert)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	resp := echo.Map{
		"bundle":         b.Bundle,
		"ca_fingerprint": b.CAFingerprint,
		"cert_dir":       b.CertDir,
		"node_id":        n.ID,
		"fingerprint":    cert.Fingerprint,
		"expires_at":     cert.ExpiresAt,
		"rotated":        true,
		"pushed":         false,
	}
	if pushErr != nil {
		resp["push_error"] = pushErr.Error()
	}
	if deprecated {
		resp["deprecated_endpoint"] = true
		resp["use"] = "/api/nodes/" + c.Param("id") + "/cert/rotate"
	}
	return c.JSON(http.StatusOK, resp)
}

func (h *Handlers) enrollNode(c echo.Context, id uuid.UUID) error {
	if h.Enrollment == nil || h.NodeCerts == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "per-node enrollment not configured")
	}
	ctx := c.Request().Context()
	n, err := h.Nodes.Get(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "node not found")
	}
	cert, err := h.NodeCerts.Ensure(ctx, n)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	b, err := h.Enrollment.BundleFor(cert)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{
		"bundle":         b.Bundle,
		"ca_fingerprint": b.CAFingerprint,
		"cert_dir":       b.CertDir,
		"node_id":        n.ID,
		"fingerprint":    cert.Fingerprint,
		"expires_at":     cert.ExpiresAt,
	})
}

// TestNodeConnection performs an on-demand dial + health check for one node.
func (h *Handlers) TestNodeConnection(c echo.Context) error {
	if h.Hub == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "hub not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	n, err := h.Nodes.Get(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "node not found")
	}
	d := h.Hub.TestConnect(c.Request().Context(), n)
	var caFP string
	if h.Enrollment != nil {
		if b, berr := h.Enrollment.Bundle(); berr == nil {
			caFP = b.CAFingerprint
		}
	}
	phase := domain.NodePhasePending
	if d.Code == domain.NodeDiagOK {
		phase = domain.NodePhaseSynced
	} else if d.CAMatch || d.Code == domain.NodeDiagCoreDown {
		phase = domain.NodePhaseConnected
	}
	return c.JSON(http.StatusOK, echo.Map{
		"diagnostics":          d,
		"panel_ca_fingerprint": caFP,
		"ca_match":             d.CAMatch,
		"enrollment_phase":     phase,
	})
}

// GetNodeDebugBundle returns a copy-friendly debug summary for support.
func (h *Handlers) GetNodeDebugBundle(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	n, err := h.Nodes.Get(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "node not found")
	}
	enrichNode(n, h)
	var caFP string
	if h.Enrollment != nil {
		if b, berr := h.Enrollment.Bundle(); berr == nil {
			caFP = b.CAFingerprint
		}
	}
	diag := "unknown"
	if n.Diagnostics != nil {
		diag = string(n.Diagnostics.Code)
	}
	netOK := false
	caMatch := false
	if n.Diagnostics != nil {
		netOK = n.Diagnostics.NetworkReachable
		caMatch = n.Diagnostics.CAMatch
	}
	return c.JSON(http.StatusOK, echo.Map{
		"node":             n.Name,
		"address":          n.Address,
		"status":           n.Status,
		"enrollment_phase": n.EnrollmentPhase,
		"diagnostics":      n.Diagnostics,
		"panel_ca_fp":      caFP,
		"last_seen":        n.LastSeen,
		"core_running":     n.Health.CoreRunning,
		"agent_version":    n.AgentVer,
		"core_version":     n.CoreVer,
		"debug_text":       formatNodeDebug(n, caFP, diag, netOK, caMatch),
	})
}

func formatNodeDebug(n *domain.Node, caFP, diag string, netOK, caMatch bool) string {
	msg := ""
	if n.Diagnostics != nil {
		msg = n.Diagnostics.Message
	}
	return "VortexUI node debug\n" +
		"  name: " + n.Name + "\n" +
		"  address: " + n.Address + "\n" +
		"  status: " + string(n.Status) + "\n" +
		"  enrollment: " + string(n.EnrollmentPhase) + "\n" +
		"  diagnostics: " + diag + "\n" +
		"  message: " + msg + "\n" +
		"  network_reachable: " + boolStr(netOK) + "\n" +
		"  ca_match: " + boolStr(caMatch) + "\n" +
		"  panel CA fingerprint: " + caFP + "\n" +
		"  core_running: " + boolStr(n.Health.CoreRunning) + "\n"
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// UpgradeNodeAgent swaps the agent binary on a node for the one this panel
// ships. The node validates it and restarts itself; nothing else changes.
func (h *Handlers) UpgradeNodeAgent(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	if h.PublicURL == "" {
		return echo.NewHTTPError(http.StatusConflict,
			"VORTEX_PUBLIC_URL is not set; the node needs a URL it can download the agent from")
	}
	url := strings.TrimRight(h.PublicURL, "/") + "/downloads/agent/vortex-node"
	ctx := c.Request().Context()
	if err := h.Hub.UpgradeAgent(ctx, id, url, ""); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{"ok": true, "message": "agent upgraded; it will reconnect in a few seconds"})
}

type installCoreRequest struct {
	Core      string `json:"core"`
	VersionID string `json:"version_id"`
	// Force lets an operator install a build that cannot meter, when the node is
	// knowingly not used for quota.
	Force bool `json:"force"`
}

// InstallNodeCore swaps a node's core binary to a catalog version.
//
// Two guards matter here: the artifact must match the node's architecture, and
// an unmetered sing-box build must not silently replace a metered one — that is
// the exact way a working quota system turns into zeroed counters.
func (h *Handlers) InstallNodeCore(c echo.Context) error {
	if h.NodeCerts == nil && h.CoreCatalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	if h.CoreCatalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	var req installCoreRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	versionID, err := uuid.Parse(req.VersionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid version_id")
	}
	ctx := c.Request().Context()
	n, err := h.Nodes.Get(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "node not found")
	}
	// The stored row carries no live health; merge it so the architecture guard
	// below can actually compare architectures.
	enrichNode(n, h)
	row, err := h.CoreCatalog.Get(ctx, versionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "version not found")
	}
	if req.Core != "" && req.Core != row.Core {
		return echo.NewHTTPError(http.StatusBadRequest, "version does not belong to that core")
	}
	if !row.Usable() {
		return echo.NewHTTPError(http.StatusConflict, "that artifact is not ready")
	}
	// Architecture must match: installing an amd64 binary on an arm64 node
	// produces a node that cannot start.
	if arch := n.Health.Arch; arch != "" && row.Arch != "" && arch != row.Arch {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("artifact is %s but the node is %s", row.Arch, arch))
	}
	if !row.Verified() && !req.Force {
		return echo.NewHTTPError(http.StatusConflict,
			"this vendor artifact has no published checksum; resend with force=true to install it unverified")
	}
	if row.Core == "singbox" && !row.Metered && !req.Force {
		return echo.NewHTTPError(http.StatusConflict,
			"this build cannot count traffic per user; pick a metered build or resend with force=true")
	}
	base := strings.TrimRight(h.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if c.Request().TLS != nil || c.Request().Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + c.Request().Host
	}
	url := row.URL
	if !strings.HasPrefix(url, "http") {
		url = base + row.URL
	}
	// Vendor rows have no checksum (GitHub publishes none); built rows always do.
	if err := h.Hub.InstallCore(ctx, n.ID, row.Core, row.Version, url, row.SHA256, row.SHA256 == ""); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{
		"core":    row.Core,
		"version": row.Version,
		"arch":    row.Arch,
		"metered": row.Metered,
	})
}
