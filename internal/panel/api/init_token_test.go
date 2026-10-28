package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestRequireInitToken proves the first-run admin endpoint is closed unless the
// caller presents the configured token, and open when it does.
func TestRequireInitToken(t *testing.T) {
	e := echo.New()
	e.POST("/admin/init", func(c echo.Context) error {
		return c.NoContent(http.StatusCreated)
	}, requireInitToken("s3cret-token"))

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusForbidden},
		{"wrong token", "nope", http.StatusForbidden},
		{"empty-token caller", "", http.StatusForbidden},
		{"correct token", "s3cret-token", http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/init", nil)
			if tc.header != "" {
				req.Header.Set("X-Vortex-Init-Token", tc.header)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
