package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenClawBillingAuthRequiresOnlyIndependentBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	router := gin.New()
	router.POST("/internal/openclaw/v1/usage-events:batch", gin.HandlerFunc(NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32)},
	})), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name       string
		authority  string
		apiKey     string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "browser jwt", authority: "Bearer eyJhbGciOiJIUzI1NiJ9.fake.jwt", wantStatus: http.StatusUnauthorized},
		{name: "gateway api key", authority: "Bearer sk-sub2api-not-the-bridge", wantStatus: http.StatusUnauthorized},
		{name: "x api key only", apiKey: "sk-sub2api-not-the-bridge", wantStatus: http.StatusUnauthorized},
		{name: "bridge bearer", authority: "Bearer " + bearer, wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:batch", nil)
			request.Header.Set("Authorization", test.authority)
			request.Header.Set("X-API-Key", test.apiKey)
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.wantStatus, recorder.Code)
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		})
	}
}

func TestOpenClawBillingAuthHidesDisabledFeature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/internal/openclaw/v1/usage-events:batch", gin.HandlerFunc(NewOpenClawBillingAuth(&config.Config{})), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:batch", nil)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 32))
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestOpenClawBillingAuthFailsClosedWhenIdentityHMACKeyIsMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/internal/openclaw/v1/usage-events:batch", gin.HandlerFunc(NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: strings.Repeat("b", 32)},
	})), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:batch", nil)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 32))
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}
