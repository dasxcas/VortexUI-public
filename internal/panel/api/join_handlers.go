package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/service"
)

// JoinHandlers serves one-command node enrollment.
type JoinHandlers struct {
	Join *service.NodeJoin
}

// CreateJoinLink mints a single-use install command for one node.
func (h *JoinHandlers) CreateJoinLink(c echo.Context) error {
	if h.Join == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "node join is not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	link, err := h.Join.Create(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "node not found")
		}
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return c.JSON(http.StatusOK, link)
}

// RedeemJoinLink serves the installer script. This endpoint is deliberately
// unauthenticated — the token in the path is the credential — and consumes the
// token on the first fetch.
func (h *JoinHandlers) RedeemJoinLink(c echo.Context) error {
	if h.Join == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "node join is not configured")
	}
	script, err := h.Join.Redeem(c.Request().Context(), c.Param("token"))
	if err != nil {
		// A used, expired or unknown token all look the same from outside.
		return c.String(http.StatusGone,
			"this install link is no longer valid (used, expired, or unknown)\ngenerate a new one in the panel: Nodes → ⚙ → Install command\n")
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(script))
}

// RevokeJoinLinks makes every outstanding link for a node useless.
func (h *JoinHandlers) RevokeJoinLinks(c echo.Context) error {
	if h.Join == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "node join is not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	n, err := h.Join.RevokeAll(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{"revoked": n})
}

// UninstallScript is served to an operator who wants to remove an agent. It is
// static and contains no credentials: the machine still has to be reachable.
func (h *JoinHandlers) UninstallScript(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", []byte(uninstallScript))
}

const uninstallScript = `#!/usr/bin/env bash
# Remove the VortexUI node agent from this machine. Run as root.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

if command -v systemctl >/dev/null 2>&1 && [ -f /etc/systemd/system/vortexui-node.service ]; then
  systemctl disable --now vortexui-node || true
  rm -f /etc/systemd/system/vortexui-node.service
  systemctl daemon-reload
fi

rm -f /usr/local/bin/vortex-node /usr/local/bin/vortex-node.bak
rm -rf /etc/vortex
echo "agent removed."
echo "The core binary (/usr/local/bin/xray or /usr/local/bin/sing-box) was left in place;"
echo "delete it yourself if nothing else on this machine uses it."
echo "Delete the node in the panel, or it will keep reporting as unreachable."
`

// DownloadAgent serves the agent binary this panel was built with.
func (h *JoinHandlers) DownloadAgent(c echo.Context) error {
	if h.Join == nil || !h.Join.AgentAvailable() {
		return echo.NewHTTPError(http.StatusNotFound,
			"this panel image does not carry a node agent; build one with CGO_ENABLED=0 go build -o vortex-node ./cmd/node")
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="vortex-node"`)
	return c.File(h.Join.AgentPath())
}
