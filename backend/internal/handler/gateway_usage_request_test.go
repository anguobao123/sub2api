package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type requestUsageRepo struct {
	service.UsageLogRepository
	lookup func(int64, string) (*service.APIKeyRequestUsage, error)
}

func (r *requestUsageRepo) GetAPIKeyRequestUsage(_ context.Context, keyID int64, requestID string) (*service.APIKeyRequestUsage, error) {
	return r.lookup(keyID, requestID)
}

func TestAPIKeyRequestUsageHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, query, credential string
		wantStatus              int
		wantBody                string
		wantLookup              bool
	}{
		{"ready", "request_id=native-request", "owner", 200, `{"ready":true,"requestId":"native-request","model":"gpt-5.5","inputTokens":10,"outputTokens":20,"cacheReadTokens":30,"cacheCreationTokens":4,"serviceTier":null,"actualCostUsd":"0.0000000123"}`, true},
		{"other-key", "request_id=native-request", "other", 200, `{"ready":false}`, true},
		{"pending", "request_id=not-recorded", "owner", 200, `{"ready":false}`, true},
		{"anonymous", "request_id=native-request", "", 401, "", false},
		{"missing-request", "", "owner", 400, "", false},
		{"invalid-request", "request_id=bad%0Arequest", "owner", 400, "", false},
		{"database-error", "request_id=database-error", "owner", 503, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			repo := &requestUsageRepo{lookup: func(keyID int64, requestID string) (*service.APIKeyRequestUsage, error) {
				called = true
				require.Contains(t, []int64{7, 8}, keyID)
				if requestID == "client:database-error" {
					return nil, errors.New("private database connection details")
				}
				if keyID != 7 || requestID != "client:native-request" {
					return nil, nil
				}
				return &service.APIKeyRequestUsage{Model: "gpt-5.5", InputTokens: 10, OutputTokens: 20,
					CacheReadTokens: 30, CacheCreationTokens: 4, ActualCostUSD: "0.0000000123"}, nil
			}}
			h := &GatewayHandler{usageService: service.NewUsageService(repo, nil, nil, nil)}
			router := gin.New()
			router.GET("/v1/usage/requests", func(c *gin.Context) {
				if tc.credential != "" {
					keyID := int64(7)
					if tc.credential == "other" {
						keyID = 8
					}
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: keyID})
				}
				h.UsageRequest(c)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/usage/requests?"+tc.query, nil))
			require.Equal(t, tc.wantStatus, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.Equal(t, tc.wantLookup, called)
			require.NotContains(t, response.Body.String(), "private database connection details")
			if tc.wantBody != "" {
				require.JSONEq(t, tc.wantBody, response.Body.String())
			}
		})
	}
}
