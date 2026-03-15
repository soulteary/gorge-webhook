package httpapi

import (
	"net/http"

	"github.com/soulteary/gorge-webhook/internal/delivery"

	"github.com/labstack/echo/v4"
)

type Deps struct {
	Store *delivery.Store
	Token string
}

type apiResponse struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func RegisterRoutes(e *echo.Echo, deps *Deps) {
	e.GET("/", healthPing())
	e.GET("/healthz", healthPing())

	g := e.Group("/api/webhook")
	g.Use(tokenAuth(deps))

	g.GET("/stats", stats(deps))
	g.GET("/hooks", listHooks(deps))
}

func tokenAuth(deps *Deps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if deps.Token == "" {
				return next(c)
			}
			token := c.Request().Header.Get("X-Service-Token")
			if token == "" {
				token = c.QueryParam("token")
			}
			if token == "" || token != deps.Token {
				return c.JSON(http.StatusUnauthorized, &apiResponse{
					Error: &apiError{Code: "ERR_UNAUTHORIZED", Message: "missing or invalid service token"},
				})
			}
			return next(c)
		}
	}
}

func healthPing() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	}
}

func respondOK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, &apiResponse{Data: data})
}

func respondErr(c echo.Context, status int, code, msg string) error {
	return c.JSON(status, &apiResponse{
		Error: &apiError{Code: code, Message: msg},
	})
}

func stats(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		s, err := deps.Store.Stats(c.Request().Context())
		if err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, s)
	}
}

func listHooks(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		row := deps.Store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM herald_webhook`)
		var count int64
		if err := row.Scan(&count); err != nil {
			return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		}
		return respondOK(c, map[string]int64{"total": count})
	}
}
