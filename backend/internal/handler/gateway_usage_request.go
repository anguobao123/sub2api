package handler

import (
	"net/http"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

var usageClientRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// UsageRequest returns only the authenticated key's record for the native
// X-Client-Request-ID response header. It does not expose another key's usage.
func (h *GatewayHandler) UsageRequest(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	requestID := c.Query("request_id")
	if !usageClientRequestIDPattern.MatchString(requestID) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "A valid X-Client-Request-ID value is required")
		return
	}
	if h.usageService == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "usage_unavailable", "Request usage is temporarily unavailable")
		return
	}
	usage, err := h.usageService.GetAPIKeyRequestUsage(c.Request.Context(), apiKey.ID, requestID)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "usage_unavailable", "Request usage is temporarily unavailable")
		return
	}
	c.JSON(http.StatusOK, struct {
		Ready bool `json:"ready"`
		*service.APIKeyRequestUsage
	}{Ready: usage != nil, APIKeyRequestUsage: usage})
}
