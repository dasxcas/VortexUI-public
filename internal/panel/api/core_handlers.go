package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vortexui/vortexui/internal/panel/service"
)

// CoreHandlers serves the core binary catalog.
type CoreHandlers struct {
	Catalog *service.CoreCatalog
	Build   bool // whether this install can compile metered builds
}

// ListCores returns every known core artifact.
func (h *CoreHandlers) ListCores(c echo.Context) error {
	if h.Catalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	rows, err := h.Catalog.List(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{"versions": rows, "build_enabled": h.Build})
}

// RefreshCores re-reads the upstream release lists.
func (h *CoreHandlers) RefreshCores(c echo.Context) error {
	if h.Catalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 120*time.Second)
	defer cancel()
	n, err := h.Catalog.Refresh(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusOK, echo.Map{"recorded": n})
}

// BuildCore compiles one catalog entry in the background. The caller polls the
// list for status; a build takes minutes and must not hold an HTTP request.
func (h *CoreHandlers) BuildCore(c echo.Context) error {
	if h.Catalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	if !h.Build {
		return echo.NewHTTPError(http.StatusConflict,
			"building is disabled on this install (set VORTEX_CORE_BUILD=true and install Docker)")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	go func() {
		_ = h.Catalog.Build(context.Background(), id)
	}()
	return c.JSON(http.StatusAccepted, echo.Map{"status": "building"})
}

// DownloadCore serves a built artifact so nodes can install it without reaching
// the internet. The file name is validated against traversal.
func (h *CoreHandlers) DownloadCore(c echo.Context) error {
	if h.Catalog == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "core catalog not configured")
	}
	name := c.Param("file")
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid file")
	}
	path := filepath.Join(h.Catalog.DataDir(), "cores", filepath.Base(name))
	return c.File(path)
}
