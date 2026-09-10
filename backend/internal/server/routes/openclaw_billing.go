package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// RegisterOpenClawBillingRoutes is deliberately outside /api/v1 and /v1. Its
// sole authentication mechanism is the independently configured internal
// bearer; normal panel JWT, admin API-key, and gateway API-key middleware are
// never installed on this group.
func RegisterOpenClawBillingRoutes(r *gin.Engine, h *handler.OpenClawBillingHandler, auth middleware.OpenClawBillingAuth) {
	if r == nil || h == nil || auth == nil {
		return
	}
	bridge := r.Group("/internal/openclaw/v1")
	bridge.Use(gin.HandlerFunc(auth))
	{
		bridge.POST("/accounts:resolve", exactOpenClawAction("resolve", h.ResolveAccount))
		bridge.POST("/task-budget-leases:reserve", exactOpenClawAction("reserve", h.ReserveLease))
		bridge.POST("/task-budget-leases/:leaseId/capture", h.CaptureLease)
		bridge.POST("/task-budget-leases/:leaseId/release", h.ReleaseLease)
		bridge.POST("/usage-events:batch", exactOpenClawAction("batch", h.IngestUsageEvents))
	}
}

// Gin uses ':' to introduce route parameters. The Suite-compatible endpoint
// includes a literal action suffix, so verify the captured tail before running
// the handler rather than accepting arbitrary /usage-events<suffix> paths.
func exactOpenClawAction(name string, next gin.HandlerFunc) gin.HandlerFunc {
	expected := ":" + name
	return func(c *gin.Context) {
		if c.Param(name) != expected {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		next(c)
	}
}
