package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

// OpenClawBillingAuth authenticates only the bridge's dedicated bearer. It is
// intentionally independent from browser JWT, admin credentials, and gateway
// API keys. The caller receives no signal about which part failed.
type OpenClawBillingAuth gin.HandlerFunc

func NewOpenClawBillingAuth(cfg *config.Config) OpenClawBillingAuth {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if cfg == nil || !cfg.OpenClawBilling.Enabled || len(cfg.OpenClawBilling.InternalBearer) < 32 || len(cfg.OpenClawBilling.IdentityHMACKey) < 32 {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		const prefix = "Bearer "
		authorization := c.GetHeader("Authorization")
		if !strings.HasPrefix(authorization, prefix) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		provided := authorization[len(prefix):]
		if len(provided) != len(cfg.OpenClawBilling.InternalBearer) ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.OpenClawBilling.InternalBearer)) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}
