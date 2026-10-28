package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/service"
)

// RelayHandlers serves CDN/Relay chain management endpoints.
type RelayHandlers struct {
	Relay  *service.RelayService
	Health *service.ChainHealthService
}

type createChainRequest struct {
	Name   string           `json:"name"`
	NodeID string           `json:"node_id"`
	Hops   []domain.RelayHop `json:"hops"`
}

// CreateChain creates a new relay chain.
func (h *RelayHandlers) CreateChain(c echo.Context) error {
	var req createChainRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid node_id")
	}
	chain, warning, err := h.Relay.CreateChain(c.Request().Context(), service.CreateChainInput{
		Name:   req.Name,
		NodeID: nodeID,
		Hops:   req.Hops,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	resp := echo.Map{"chain": chain}
	if warning != "" {
		resp["warning"] = warning
	}
	return c.JSON(http.StatusCreated, resp)
}

// ListChains lists all relay chains.
func (h *RelayHandlers) ListChains(c echo.Context) error {
	chains, err := h.Relay.ListChains(c.Request().Context())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "list failed")
	}
	resp := echo.Map{"chains": chains}
	if h.Health != nil {
		health := map[string]service.ChainHealth{}
		for id, v := range h.Health.Snapshot() {
			health[id.String()] = v
		}
		resp["health"] = health
	}
	return c.JSON(http.StatusOK, resp)
}

// ProbeChain runs a chain's health check on demand and returns the fresh result.
func (h *RelayHandlers) ProbeChain(c echo.Context) error {
	if h.Health == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "health checks are not configured")
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()
	health, applicable, err := h.Health.CheckID(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "chain not found")
	}
	if !applicable {
		return echo.NewHTTPError(http.StatusBadRequest, "this chain has no managed path to check")
	}
	return c.JSON(http.StatusOK, echo.Map{"health": health})
}

type updateChainRequest struct {
	Name    string            `json:"name"`
	Hops    []domain.RelayHop `json:"hops"`
	Enabled bool              `json:"enabled"`
}

// UpdateChain updates a relay chain.
func (h *RelayHandlers) UpdateChain(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	var req updateChainRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	chain, warning, err := h.Relay.UpdateChain(c.Request().Context(), id, req.Name, req.Hops, req.Enabled)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	resp := echo.Map{"chain": chain}
	if warning != "" {
		resp["warning"] = warning
	}
	return c.JSON(http.StatusOK, resp)
}

// DeleteChain removes a relay chain.
func (h *RelayHandlers) DeleteChain(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	if err := h.Relay.DeleteChain(c.Request().Context(), id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// ApplyChain re-runs provisioning for a chain, e.g. after fixing a port clash.
func (h *RelayHandlers) ApplyChain(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid id")
	}
	chain, warning, err := h.Relay.ApplyChain(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	resp := echo.Map{"chain": chain}
	if warning != "" {
		resp["warning"] = warning
	}
	return c.JSON(http.StatusOK, resp)
}
