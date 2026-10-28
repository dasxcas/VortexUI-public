package api

import (
	"strings"

	"github.com/labstack/echo/v4"
)

// commercialPrefixes are the API surfaces that exist only for selling access.
// In personal mode they answer 404 rather than 403: they are not disabled for
// this account, they simply are not part of this installation.
var commercialPrefixes = []string{
	"/api/plans",
	"/api/orders",
	"/api/shop",
	"/api/payment",
	"/api/tickets",
	"/api/referrals",
	"/api/account/wallet",
	"/api/account/payment-config",
	"/api/reseller",
	"/api/billing",
}

// personalModeGuard blocks the commercial API surface.
func personalModeGuard() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := c.Request().URL.Path
			for _, prefix := range commercialPrefixes {
				if p == prefix || strings.HasPrefix(p, prefix+"/") {
					return echo.ErrNotFound
				}
			}
			// Per-reseller wallet/plan endpoints live under an id segment.
			if strings.HasPrefix(p, "/api/admins/") &&
				(strings.HasSuffix(p, "/wallet") || strings.HasSuffix(p, "/plans")) {
				return echo.ErrNotFound
			}
			// The subscription page's shop tab.
			if strings.HasPrefix(p, "/sub/") && strings.HasSuffix(p, "/shop") {
				return echo.ErrNotFound
			}
			return next(c)
		}
	}
}
